//go:build windows

package console

import "fmt"

func interruptSelf() error {
	return fmt.Errorf("SIGINT self-signal not supported in this test on Windows")
}
