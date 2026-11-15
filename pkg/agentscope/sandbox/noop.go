package sandbox

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// NoopSandbox executes commands without any sandboxing restrictions.
type NoopSandbox struct{}

// Type returns "noop" identifying this as a no-op sandbox.
func (NoopSandbox) Type() string { return "noop" }

// Available always returns true since no-op execution requires no dependencies.
func (NoopSandbox) Available() bool { return true }

// Execute runs the command directly on the host without sandboxing.
func (NoopSandbox) Execute(ctx context.Context, req *ExecRequest) (*ExecResult, error) {
	start := time.Now()

	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	args := req.Args
	cmd := exec.CommandContext(ctx, req.Command, args...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := &ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, err
	}
	return result, nil
}

// Setup is a no-op; the sandbox has no state to initialize.
func (NoopSandbox) Setup(_ context.Context, _ Policy) error { return nil } //nolint:gocritic // interface

// Teardown is a no-op; the sandbox has no state to clean up.
func (NoopSandbox) Teardown(_ context.Context) error { return nil }

type noopProvider struct{}

func (noopProvider) Name() string                             { return "noop" }
func (noopProvider) Priority() int                            { return 0 }
func (noopProvider) Available() bool                          { return true }
func (noopProvider) Create(_ map[string]any) (Sandbox, error) { return NoopSandbox{}, nil }

func init() {
	RegisterProvider(noopProvider{})
}
