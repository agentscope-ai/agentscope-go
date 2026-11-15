package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DockerBackend executes commands inside a Docker container.
type DockerBackend struct {
	ContainerID string
	Timeout     time.Duration
}

func (b *DockerBackend) ExecCommand(ctx context.Context, command string) (*ExecResult, error) {
	timeout := b.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "exec", b.ContainerID, "/bin/sh", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("docker exec: %w", err)
		}
	}

	return &ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, nil
}

// DockerWorkspaceConfig configures a Docker-based workspace.
type DockerWorkspaceConfig struct {
	Image          string
	ContainerName  string
	WorkDir        string   // container working directory
	Mounts         []string // host:container mount pairs
	CommandTimeout time.Duration
}

// DockerWorkspace provides file and command sandboxing inside a Docker container.
type DockerWorkspace struct {
	containerID string
	workDir     string
	backend     *DockerBackend
}

// NewDockerWorkspace creates and starts a Docker container for the workspace.
func NewDockerWorkspace(ctx context.Context, cfg *DockerWorkspaceConfig) (*DockerWorkspace, error) {
	if cfg.Image == "" {
		cfg.Image = "ubuntu:latest"
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = "/workspace"
	}

	args := []string{"run", "-d", "--rm", "-w", cfg.WorkDir}
	if cfg.ContainerName != "" {
		args = append(args, "--name", cfg.ContainerName)
	}
	for _, m := range cfg.Mounts {
		args = append(args, "-v", m)
	}
	args = append(args, cfg.Image, "sleep", "infinity")

	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker run: %s: %w", stderr.String(), err)
	}

	containerID := strings.TrimSpace(stdout.String())
	backend := &DockerBackend{ContainerID: containerID, Timeout: cfg.CommandTimeout}

	return &DockerWorkspace{
		containerID: containerID,
		workDir:     cfg.WorkDir,
		backend:     backend,
	}, nil
}

func (w *DockerWorkspace) WriteFile(ctx context.Context, path string, data []byte) error {
	fullPath := w.resolvePath(path)
	// Create parent directory
	dir := fullPath[:strings.LastIndex(fullPath, "/")]
	if _, err := w.backend.ExecCommand(ctx, fmt.Sprintf("mkdir -p %q", dir)); err != nil {
		return err
	}
	// Write via stdin pipe
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", w.containerID, "sh", "-c", fmt.Sprintf("cat > %q", fullPath))
	cmd.Stdin = bytes.NewReader(data)
	return cmd.Run()
}

func (w *DockerWorkspace) ReadFile(ctx context.Context, path string) ([]byte, error) {
	fullPath := w.resolvePath(path)
	result, err := w.backend.ExecCommand(ctx, fmt.Sprintf("cat %q", fullPath))
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("read file: %s", result.Stderr)
	}
	return []byte(result.Stdout), nil
}

func (w *DockerWorkspace) ListFiles(ctx context.Context, dir string) ([]FileInfo, error) {
	fullDir := w.resolvePath(dir)
	result, err := w.backend.ExecCommand(ctx, fmt.Sprintf("find %q -maxdepth 1 -printf '%%y %%s %%P\\n' 2>/dev/null || ls -la %q", fullDir, fullDir))
	if err != nil {
		return nil, err
	}
	var files []FileInfo
	for _, line := range strings.Split(result.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 3 || parts[2] == "" {
			continue
		}
		isDir := parts[0] == "d"
		var size int64
		_, _ = fmt.Sscanf(parts[1], "%d", &size)
		files = append(files, FileInfo{
			Name:  parts[2],
			Path:  dir + "/" + parts[2],
			IsDir: isDir,
			Size:  size,
		})
	}
	return files, nil
}

func (w *DockerWorkspace) RemoveFile(ctx context.Context, path string) error {
	fullPath := w.resolvePath(path)
	result, err := w.backend.ExecCommand(ctx, fmt.Sprintf("rm -f %q", fullPath))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("remove file: %s", result.Stderr)
	}
	return nil
}

func (w *DockerWorkspace) Execute(ctx context.Context, command string) (*ExecResult, error) {
	return w.backend.ExecCommand(ctx, command)
}

func (w *DockerWorkspace) BasePath() string { return w.workDir }

// Close stops and removes the Docker container.
func (w *DockerWorkspace) Close(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", w.containerID)
	return cmd.Run()
}

// ExecPath implements ExecPathResolver: this backend's Execute runs the
// command with the caller's path resolved the same way ReadFile resolves it,
// which for this workspace means the absolute in-sandbox path. A relative path
// would be resolved by the shell against the image/sandbox working directory
// instead, i.e. against a different file.
func (w *DockerWorkspace) ExecPath(callerPath string) string { return w.resolvePath(callerPath) }

func (w *DockerWorkspace) resolvePath(path string) string {
	if strings.HasPrefix(path, "/") {
		return path
	}
	return w.workDir + "/" + path
}
