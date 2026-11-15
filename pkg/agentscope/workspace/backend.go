package workspace

import (
	"bytes"
	"context"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/platform"
	"os/exec"
	"time"
)

// Backend abstracts command execution for different environments.
type Backend interface {
	ExecCommand(ctx context.Context, command string) (*ExecResult, error)
}

// LocalBackend executes commands locally via /bin/sh.
type LocalBackend struct {
	WorkDir string
	Timeout time.Duration // default: 30s
}

func (b *LocalBackend) ExecCommand(ctx context.Context, command string) (*ExecResult, error) {
	timeout := b.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := platform.Command(ctx, command)
	if b.WorkDir != "" {
		cmd.Dir = b.WorkDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, err
		}
	}

	return &ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, nil
}
