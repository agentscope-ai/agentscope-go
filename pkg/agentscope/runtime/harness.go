package runtime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/logging"
)

// RunOption configures the Run function.
type RunOption func(*runConfig)

type runConfig struct {
	shutdownTimeout time.Duration
	healthAddr      string
	pidFile         string
	panicRecovery   bool
	onShutdown      func()
}

// WithGracefulShutdown sets the timeout for graceful shutdown after receiving
// SIGTERM or SIGINT. Defaults to 30 seconds.
func WithGracefulShutdown(timeout time.Duration) RunOption {
	return func(c *runConfig) { c.shutdownTimeout = timeout }
}

// WithHealthProbe starts an HTTP server at addr exposing /healthz and /readyz.
func WithHealthProbe(addr string) RunOption {
	return func(c *runConfig) { c.healthAddr = addr }
}

// WithPIDFile writes the process PID to the given file path and removes it on
// exit.
func WithPIDFile(path string) RunOption {
	return func(c *runConfig) { c.pidFile = path }
}

// WithPanicRecovery wraps each conversation loop iteration in a
// recover/continue, logging panics instead of crashing.
func WithPanicRecovery() RunOption {
	return func(c *runConfig) { c.panicRecovery = true }
}

// WithOnShutdown registers a callback invoked after the conversation loop
// exits and before Run returns.
func WithOnShutdown(fn func()) RunOption {
	return func(c *runConfig) { c.onShutdown = fn }
}

// Runnable is the interface that Run drives. Both ConversationLoop and custom
// implementations can satisfy it.
type Runnable interface {
	Run(ctx context.Context) error
}

// Run starts a managed agent process around the given Runnable (typically a
// ConversationLoop). It handles OS signals, optional health probes, PID file
// management, and panic recovery.
func Run(ctx context.Context, r Runnable, opts ...RunOption) error {
	cfg := &runConfig{
		shutdownTimeout: 30 * time.Second,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	if cfg.pidFile != "" {
		if err := writePIDFile(cfg.pidFile); err != nil {
			return fmt.Errorf("runtime: pid file: %w", err)
		}
		defer removePIDFile(cfg.pidFile)
	}

	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	// Health probe server.
	var healthSrv *http.Server
	if cfg.healthAddr != "" {
		healthSrv = newHealthServer(cfg.healthAddr)
		ln, err := net.Listen("tcp", cfg.healthAddr)
		if err != nil {
			return fmt.Errorf("runtime: health probe: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := healthSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
				logging.Warn("health probe serve error", logging.Err(err))
			}
		}()
	}

	// Run the main loop.
	var runErr error
	if cfg.panicRecovery {
		runErr = runWithRecovery(sigCtx, r)
	} else {
		runErr = r.Run(sigCtx)
	}

	// Context errors from signal handling are normal shutdown, not failures.
	if runErr != nil && sigCtx.Err() != nil {
		runErr = nil
	}

	// Shutdown phase.
	stop() // stop receiving signals

	if healthSrv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), cfg.shutdownTimeout)
		defer cancel()
		_ = healthSrv.Shutdown(shutCtx)
	}

	if cfg.onShutdown != nil {
		cfg.onShutdown()
	}

	wg.Wait()
	return runErr
}

func runWithRecovery(ctx context.Context, r Runnable) error {
	for {
		recovered := false
		err := func() (retErr error) {
			defer func() {
				if p := recover(); p != nil {
					logging.Error(fmt.Sprintf("runtime: recovered panic: %v", p))
					recovered = true
					retErr = nil
				}
			}()
			return r.Run(ctx)
		}()

		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if !recovered {
			return nil
		}
	}
}

func newHealthServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return &http.Server{Addr: addr, Handler: mux}
}

func writePIDFile(path string) error {
	return os.WriteFile(path, []byte(fmt.Sprintf("%d", os.Getpid())), 0o644)
}

func removePIDFile(path string) {
	_ = os.Remove(path)
}
