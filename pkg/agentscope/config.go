package agentscope

import (
	"io"
	"log"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/logging"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// Config corresponds to the Python _ConfigCls and holds global runtime settings.
type Config struct {
	RunID        string
	Project      string
	Name         string
	CreatedAt    time.Time
	TraceEnabled bool

	LoggingPath  string
	LoggingLevel string

	StudioURL  string
	TracingURL string
}

var (
	globalCfg   Config
	globalCfgMu sync.RWMutex
	logger      = log.New(os.Stdout, "[agentscope] ", log.LstdFlags|log.Lmicroseconds)

	idMu      sync.RWMutex
	idFactory = uuid.NewString
)

// GenerateID returns a new unique identifier using the configured ID factory.
// By default it produces UUID v4 strings. Use SetIDFactory or WithIDFactory to
// plug in a custom scheme (e.g. UUID v7 for time-ordered IDs).
func GenerateID() string {
	idMu.RLock()
	f := idFactory
	idMu.RUnlock()
	return f()
}

// SetIDFactory replaces the global ID generation function.
func SetIDFactory(f func() string) {
	if f == nil {
		panic("agentscope: SetIDFactory called with nil factory")
	}
	idMu.Lock()
	idFactory = f
	idMu.Unlock()
}

// Option is the functional option type for Init.
type Option func(*Config)

func WithProject(project string) Option {
	return func(c *Config) {
		c.Project = project
	}
}

func WithName(name string) Option {
	return func(c *Config) {
		c.Name = name
	}
}

func WithRunID(runID string) Option {
	return func(c *Config) {
		c.RunID = runID
	}
}

func WithLogging(path, level string) Option {
	return func(c *Config) {
		c.LoggingPath = path
		c.LoggingLevel = level
	}
}

func WithStudioURL(url string) Option {
	return func(c *Config) {
		c.StudioURL = url
	}
}

func WithTracingURL(url string) Option {
	return func(c *Config) {
		c.TracingURL = url
	}
}

// WithIDFactory sets the global ID generation function during Init.
func WithIDFactory(f func() string) Option {
	return func(c *Config) {
		SetIDFactory(f)
	}
}

// Init initializes the global agentscope configuration and logging.
// It mirrors Python agentscope.init but uses Go-style options.
func Init(opts ...Option) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	setupLogger(&cfg)
	// TODO: Studio registration and tracing initialization can be wired here later.

	globalCfgMu.Lock()
	globalCfg = cfg
	globalCfgMu.Unlock()

	logger.Printf("initialized: project=%s name=%s run_id=%s", cfg.Project, cfg.Name, cfg.RunID)
}

func defaultConfig() Config {
	now := time.Now()
	return Config{
		RunID:        GenerateID(),
		Project:      "UnnamedProject_" + now.Format("20060102"),
		Name:         now.Format("150405"),
		CreatedAt:    now,
		LoggingPath:  "",
		LoggingLevel: "INFO",
	}
}

func setupLogger(cfg *Config) {
	var out *os.File
	if cfg.LoggingPath == "" {
		// Default to stdout; logger is already initialized at package level.
		out = os.Stdout
	} else {
		f, err := os.OpenFile(cfg.LoggingPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			logger.Printf("failed to open log file %s: %v", cfg.LoggingPath, err)
			out = os.Stdout
		} else {
			out = f
		}
	}

	logger.SetOutput(out)

	slogLevel := logging.ParseLevel(cfg.LoggingLevel)
	slogHandler := slog.NewTextHandler(out, &slog.HandlerOptions{Level: slogLevel})
	logging.SetDefault(slog.New(slogHandler))

	logrus.AddHook(&logging.LogrusToSlogHook{})
	logrus.SetOutput(io.Discard)
	logrus.SetLevel(logrus.TraceLevel)
}

// ConfigSnapshot returns a copy of the current global configuration.
func ConfigSnapshot() Config {
	globalCfgMu.RLock()
	defer globalCfgMu.RUnlock()
	return globalCfg
}

// Logger returns the global logger instance.
func Logger() *log.Logger {
	return logger
}

// Log returns the global logrus logger with level support.
func Log() *logrus.Logger {
	return logrus.StandardLogger()
}

// SLog returns the global slog.Logger. Prefer this over Log() for new code.
func SLog() *slog.Logger {
	return logging.Default()
}
