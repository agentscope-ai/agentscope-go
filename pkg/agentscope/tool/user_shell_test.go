package tool

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// A timed-out command is killed (process group on Unix) and reported as
// exit code -1 with the ctx error, in bounded time. This locks the kill
// path: a regression here silently hangs the caller or orphans processes.
func TestRunUserShellTimeoutKillsProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a Unix shell")
	}
	start := time.Now()
	code, err := RunUserShell(context.Background(), "sleep 30", 200*time.Millisecond, nil)
	elapsed := time.Since(start)
	if code != -1 {
		t.Errorf("exit code = %d, want -1 for a killed command", code)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	// WaitDelay (5s) bounds Wait once the group is killed; allow slack.
	if elapsed > 8*time.Second {
		t.Errorf("kill path too slow: %v (orphaned pipe would hang Wait)", elapsed)
	}
}
