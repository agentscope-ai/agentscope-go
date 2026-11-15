package platform

import (
	"path/filepath"
	"runtime"
	"strings"
)

// NormalizePath converts a path to the OS-native format.
func NormalizePath(p string) string {
	return filepath.FromSlash(filepath.Clean(p))
}

// ToSlash converts a path to forward-slash format (for display/comparison).
func ToSlash(p string) string {
	return filepath.ToSlash(p)
}

// IsAbsolute returns true if the path is absolute on the current OS.
func IsAbsolute(p string) bool {
	if runtime.GOOS == "windows" {
		if len(p) >= 2 && p[1] == ':' {
			return true
		}
		if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
			return true
		}
	}
	return filepath.IsAbs(p)
}
