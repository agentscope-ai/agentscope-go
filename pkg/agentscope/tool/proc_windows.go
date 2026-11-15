//go:build windows

package tool

import "os/exec"

// configureProcIsolation is a no-op on Windows. exec.CommandContext's
// default Cancel (Process.Kill) terminates the process tree on Windows.
func configureProcIsolation(_ *exec.Cmd) {}

// configureProcGroup is a no-op on Windows (no process groups).
func configureProcGroup(_ *exec.Cmd) {}

// killProcessGroup terminates the process (Windows has no process groups).
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
