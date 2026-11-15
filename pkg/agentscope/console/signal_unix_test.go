//go:build !windows

package console

import (
	"os"
	"syscall"
)

func interruptSelf() error {
	return syscall.Kill(os.Getpid(), syscall.SIGINT)
}
