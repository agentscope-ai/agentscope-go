package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// AppleContainerConfig configures an AppleContainerWorkspace.
type AppleContainerConfig struct {
	Image          string
	Name           string
	CommandTimeout time.Duration // default: 30s
}

// AppleContainerWorkspace implements Workspace using Apple's container CLI tool.
type AppleContainerWorkspace struct {
	name           string
	commandTimeout time.Duration
}

// Compile-time interface check.
var _ Workspace = (*AppleContainerWorkspace)(nil)

// NewAppleContainerWorkspace creates and starts an Apple container.
func NewAppleContainerWorkspace(cfg AppleContainerConfig) (*AppleContainerWorkspace, error) {
	if cfg.Image == "" {
		return nil, fmt.Errorf("workspace: apple container image is required")
	}
	if cfg.Name == "" {
		return nil, fmt.Errorf("workspace: apple container name is required")
	}

	timeout := cfg.CommandTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	// Create and start the container.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "container", "run", "--name", cfg.Name, cfg.Image)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("workspace: create apple container: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Create the /workspace directory inside the container.
	mkdirCtx, mkdirCancel := context.WithTimeout(context.Background(), timeout)
	defer mkdirCancel()

	mkdirCmd := exec.CommandContext(mkdirCtx, "container", "exec", cfg.Name, "--", "sh", "-c", "mkdir -p /workspace")
	if out, err := mkdirCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("workspace: create /workspace dir: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return &AppleContainerWorkspace{
		name:           cfg.Name,
		commandTimeout: timeout,
	}, nil
}

// BasePath returns the fixed path inside the container.
func (w *AppleContainerWorkspace) BasePath() string { return "/workspace" }

// Execute runs a command inside the Apple container.
func (w *AppleContainerWorkspace) Execute(ctx context.Context, command string) (*ExecResult, error) {
	ctx, cancel := context.WithTimeout(ctx, w.commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "container", "exec", w.name, "--", "sh", "-c", command)
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
			return nil, fmt.Errorf("workspace: apple container exec: %w", err)
		}
	}

	return result, nil
}

// WriteFile writes data to a file inside the container via tee.
func (w *AppleContainerWorkspace) WriteFile(ctx context.Context, path string, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, w.commandTimeout)
	defer cancel()

	fullPath := w.resolvePath(path)

	// Ensure parent directory exists.
	dir := fullPath[:strings.LastIndex(fullPath, "/")]
	if dir != "" {
		mkdirCmd := exec.CommandContext(ctx, "container", "exec", w.name, "--", "sh", "-c", fmt.Sprintf("mkdir -p %q", dir))
		if out, err := mkdirCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("workspace: mkdir in container: %s: %w", strings.TrimSpace(string(out)), err)
		}
	}

	cmd := exec.CommandContext(ctx, "container", "exec", w.name, "--", "tee", fullPath)
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("workspace: write file in container: %s: %w", strings.TrimSpace(stderr.String()), err)
	}

	return nil
}

// ReadFile reads a file from the container via cat.
func (w *AppleContainerWorkspace) ReadFile(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, w.commandTimeout)
	defer cancel()

	fullPath := w.resolvePath(path)

	cmd := exec.CommandContext(ctx, "container", "exec", w.name, "--", "cat", fullPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("workspace: read file in container: %s: %w", strings.TrimSpace(stderr.String()), err)
	}

	return stdout.Bytes(), nil
}

// ListFiles lists directory contents inside the container.
func (w *AppleContainerWorkspace) ListFiles(ctx context.Context, dir string) ([]FileInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, w.commandTimeout)
	defer cancel()

	fullDir := w.resolvePath(dir)

	cmd := exec.CommandContext(ctx, "container", "exec", w.name, "--", "ls", "-la", fullDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("workspace: list files in container: %s: %w", strings.TrimSpace(stderr.String()), err)
	}

	return parseLsOutput(stdout.String(), dir), nil
}

// RemoveFile removes a file inside the container.
func (w *AppleContainerWorkspace) RemoveFile(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, w.commandTimeout)
	defer cancel()

	fullPath := w.resolvePath(path)

	cmd := exec.CommandContext(ctx, "container", "exec", w.name, "--", "rm", fullPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("workspace: remove file in container: %s: %w", strings.TrimSpace(stderr.String()), err)
	}

	return nil
}

// Close stops and removes the Apple container.
func (w *AppleContainerWorkspace) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), w.commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "container", "stop", w.name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("workspace: stop apple container: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return nil
}

// resolvePath returns the full container path for a given workspace-relative path.
// ExecPath implements ExecPathResolver: this backend's Execute runs the
// command with the caller's path resolved the same way ReadFile resolves it,
// which for this workspace means the absolute in-sandbox path. A relative path
// would be resolved by the shell against the image/sandbox working directory
// instead, i.e. against a different file.
func (w *AppleContainerWorkspace) ExecPath(callerPath string) string {
	return w.resolvePath(callerPath)
}

func (w *AppleContainerWorkspace) resolvePath(path string) string {
	if strings.HasPrefix(path, "/") {
		return path
	}
	return "/workspace/" + path
}

// parseLsOutput parses `ls -la` output into FileInfo entries.
func parseLsOutput(output string, baseDir string) []FileInfo {
	var files []FileInfo
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		// Skip total line and . / .. entries.
		if strings.HasPrefix(line, "total") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		name := strings.Join(fields[8:], " ")
		if name == "." || name == ".." {
			continue
		}

		isDir := strings.HasPrefix(fields[0], "d")
		size, _ := strconv.ParseInt(fields[4], 10, 64)

		entryPath := name
		if baseDir != "" && baseDir != "." {
			entryPath = strings.TrimSuffix(baseDir, "/") + "/" + name
		}

		files = append(files, FileInfo{
			Name:  name,
			Path:  entryPath,
			IsDir: isDir,
			Size:  size,
		})
	}
	return files
}
