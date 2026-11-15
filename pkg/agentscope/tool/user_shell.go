package tool

import (
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/platform"
)

// RunUserShell executes a user-typed shell command outside the model loop
// (codex RunUserShellCommand parity): the command runs through the detected
// platform shell with stdout/stderr merged and streamed to onOutput in
// bounded chunks. A zero timeout means no deadline beyond ctx. onOutput
// must not block: a stalled callback stops the read loop, and the child
// blocks once the pipe buffer fills (only timeout/ctx unstick it).
//
// The returned code is the process exit code, or -1 when the process was
// killed (timeout/cancel) or its status is unavailable. A non-nil error
// means the command could not be started or waited on; ctx cancellation and
// timeout surface as the ctx error with code -1.
//
// Security contract: this path is user-initiated (the host's "!" escape), so
// it runs WITHOUT the permission engine — the user approved by typing it.
// Never route model-generated commands through here.
func RunUserShell(ctx context.Context, command string, timeout time.Duration, onOutput func([]byte)) (int, error) {
	runCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := platform.Command(runCtx, command)
	configureProcIsolation(cmd) // process-group kill on timeout/cancel

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	cmd.Stderr = cmd.Stdout // merge stderr into the streamed output
	if err := cmd.Start(); err != nil {
		return -1, err
	}

	buf := make([]byte, 32*1024)
	for {
		n, readErr := stdout.Read(buf)
		if n > 0 && onOutput != nil {
			// Copy: the buffer is reused by the next Read.
			onOutput(append([]byte(nil), buf[:n]...))
		}
		if readErr != nil {
			// EOF or a pipe error: the process exit status reported by
			// Wait below is the authoritative outcome.
			break
		}
	}
	waitErr := cmd.Wait()
	if runCtx.Err() != nil {
		return -1, runCtx.Err()
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return -1, waitErr
	}
	return 0, nil
}
