//go:build !windows

package tool

import (
	"os/exec"
	"syscall"
	"time"
)

// configureProcIsolation sets up process-group isolation for a command.
// On Unix this creates a new process group via Setpgid and overrides the
// Cancel function to kill the entire group (not just the leader) when the
// context fires. WaitDelay ensures orphaned child pipes don't hang Wait.
func configureProcIsolation(cmd *exec.Cmd) {
	configureProcGroup(cmd)
	cmd.Cancel = func() error {
		// Negative PID sends the signal to every process in the group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
}

// configureProcGroup only places the command in its own process group; use
// it for plain exec.Cmd processes (persistent shell sessions) where setting
// Cancel would be rejected (Cancel requires CommandContext).
func configureProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs the command's whole process group (sessions use
// plain exec.Cmd, not CommandContext, so cancellation is manual).
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
