package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// BubblewrapConfig configures a BubblewrapWorkspace.
type BubblewrapConfig struct {
	RootDir        string
	AllowNetwork   bool
	CommandTimeout time.Duration // default: 30s
}

// BubblewrapWorkspace implements Workspace using bubblewrap (bwrap) for
// lightweight Linux sandboxing.
type BubblewrapWorkspace struct {
	rootDir        string
	allowNetwork   bool
	commandTimeout time.Duration
}

// Compile-time interface check.
var _ Workspace = (*BubblewrapWorkspace)(nil)

// NewBubblewrapWorkspace creates a workspace backed by bwrap sandboxing.
// The rootDir is created if it does not exist.
func NewBubblewrapWorkspace(cfg BubblewrapConfig) (*BubblewrapWorkspace, error) {
	if cfg.RootDir == "" {
		return nil, fmt.Errorf("workspace: bubblewrap root dir is required")
	}

	abs, err := filepath.Abs(cfg.RootDir)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve path: %w", err)
	}

	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("workspace: create root dir: %w", err)
	}

	timeout := cfg.CommandTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &BubblewrapWorkspace{
		rootDir:        abs,
		allowNetwork:   cfg.AllowNetwork,
		commandTimeout: timeout,
	}, nil
}

// BasePath returns the root directory used by the bubblewrap workspace.
func (w *BubblewrapWorkspace) BasePath() string { return w.rootDir }

// Execute runs a command inside a bwrap sandbox.
func (w *BubblewrapWorkspace) Execute(ctx context.Context, command string) (*ExecResult, error) {
	ctx, cancel := context.WithTimeout(ctx, w.commandTimeout)
	defer cancel()

	args := w.bwrapArgs()
	args = append(args, "--", "sh", "-c", command)

	cmd := exec.CommandContext(ctx, "bwrap", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := &ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else if ctx.Err() != nil {
			result.ExitCode = -1
			result.Stderr += "\ncommand timed out"
		} else {
			return nil, fmt.Errorf("workspace: bwrap exec: %w", err)
		}
	}

	return result, nil
}

// WriteFile writes data to a file within the workspace root directory.
// File operations work directly on the host filesystem since bwrap uses bind mounts.
func (w *BubblewrapWorkspace) WriteFile(ctx context.Context, path string, data []byte) error {
	resolved, err := w.resolve(path)
	if err != nil {
		return err
	}

	dir := filepath.Dir(resolved)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("workspace: create parent dir: %w", err)
	}

	return os.WriteFile(resolved, data, 0o644)
}

// ReadFile reads a file from the workspace root directory.
func (w *BubblewrapWorkspace) ReadFile(ctx context.Context, path string) ([]byte, error) {
	resolved, err := w.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(resolved)
}

// ListFiles lists entries in a directory within the workspace root.
func (w *BubblewrapWorkspace) ListFiles(ctx context.Context, dir string) ([]FileInfo, error) {
	resolved, err := w.resolve(dir)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		return nil, fmt.Errorf("workspace: list: %w", err)
	}

	var files []FileInfo
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(w.rootDir, filepath.Join(resolved, e.Name()))
		files = append(files, FileInfo{
			Name:  e.Name(),
			Path:  rel,
			IsDir: e.IsDir(),
			Size:  info.Size(),
		})
	}
	return files, nil
}

// RemoveFile removes a file from the workspace root directory.
func (w *BubblewrapWorkspace) RemoveFile(ctx context.Context, path string) error {
	resolved, err := w.resolve(path)
	if err != nil {
		return err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("workspace: stat: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("workspace: cannot remove directory %q", path)
	}

	return os.Remove(resolved)
}

// bwrapArgs builds the bwrap command-line arguments.
func (w *BubblewrapWorkspace) bwrapArgs() []string {
	args := []string{
		"--bind", w.rootDir, "/",
		"--proc", "/proc",
		"--dev", "/dev",
	}
	if !w.allowNetwork {
		args = append(args, "--unshare-net")
	}
	return args
}

// resolve converts a relative path to an absolute path under the root,
// rejecting any traversal outside the root directory.
func (w *BubblewrapWorkspace) resolve(path string) (string, error) {
	base := filepath.Clean(w.rootDir)
	var cleaned string
	if filepath.IsAbs(path) {
		cleaned = filepath.Clean(path)
	} else {
		cleaned = filepath.Join(base, path)
	}
	// Separator-aware containment (a bare prefix match would admit
	// sibling directories).
	if cleaned != base && !strings.HasPrefix(cleaned, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, path)
	}
	return cleaned, nil
}
