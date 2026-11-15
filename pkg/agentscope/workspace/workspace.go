// Package workspace provides file-system and command execution sandboxing for agents.
//
// A Workspace isolates an agent's file I/O and shell commands to a base
// directory, preventing accidental access to the broader file system.
// LocalWorkspace is the default implementation; DockerWorkspace can be added
// for container-level isolation.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/fsutil"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/platform"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Workspace defines the interface for sandboxed file and command operations.
type Workspace interface {
	WriteFile(ctx context.Context, path string, data []byte) error
	ReadFile(ctx context.Context, path string) ([]byte, error)
	ListFiles(ctx context.Context, dir string) ([]FileInfo, error)
	RemoveFile(ctx context.Context, path string) error
	Execute(ctx context.Context, command string) (*ExecResult, error)
	BasePath() string
}

// ExecPathResolver is an optional Workspace capability. It returns the path
// form that Execute's shell will resolve for a caller-supplied (normally
// workspace-relative) path — in other words, the spelling that names the SAME
// file ReadFile reads.
//
// ToolBackend.StatFile has to interpolate a path into a shell command, and the
// correct form differs per backend:
//
//   - Docker runs `docker exec <id> /bin/sh -c` with no -w, so a relative path
//     resolves against the image WORKDIR while ReadFile resolves against the
//     container workDir. The absolute form is required. Daytona and
//     AppleContainer are the same shape.
//   - K8s (`kubectl exec ... -- cat <path>`), E2B and OpenSandbox pass the
//     caller's path straight through, and bubblewrap binds its HOST root to "/",
//     so an absolute BasePath-joined path would name a file that does not exist
//     inside the sandbox. For those the relative form is required.
//
// A workspace that does not implement this gets the caller-relative path, which
// matches the majority of backends and is the historical behavior. Getting this
// wrong is not merely wasteful: a stat that lands on a different file supplies a
// freshness key for the wrong file, and the read cache can then serve stale
// content as fresh.
type ExecPathResolver interface {
	ExecPath(callerPath string) string
}

// FileInfo describes a file or directory entry.
type FileInfo struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// ExecResult holds the outcome of a command execution.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// ErrPathEscape signals that a path resolves outside the workspace jail.
// Route layers map it onto client errors via errors.Is.
var ErrPathEscape = errors.New("workspace: path escapes workspace")

// Offloader converts content into a workspace-stored file, returning its path.
type Offloader interface {
	OffloadContent(ctx context.Context, content string, filename string) (path string, err error)
	OffloadToolResult(ctx context.Context, content string, toolCallID string) (path string, err error)
}

// LocalWorkspace restricts file operations to a base directory on the local
// file system and executes commands with that directory as the working dir.
type LocalWorkspace struct {
	basePath       string
	commandTimeout time.Duration
}

// LocalConfig configures a LocalWorkspace.
type LocalConfig struct {
	BasePath       string
	CommandTimeout time.Duration // default: 30s
}

// NewLocalWorkspace creates a workspace rooted at the given path.
// The directory is created if it does not exist.
func NewLocalWorkspace(cfg LocalConfig) (*LocalWorkspace, error) {
	if cfg.BasePath == "" {
		return nil, fmt.Errorf("workspace: base path is required")
	}

	abs, err := filepath.Abs(cfg.BasePath)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve path: %w", err)
	}

	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("workspace: create dir: %w", err)
	}

	timeout := cfg.CommandTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &LocalWorkspace{
		basePath:       abs,
		commandTimeout: timeout,
	}, nil
}

// BasePath returns the workspace root directory.
func (w *LocalWorkspace) BasePath() string { return w.basePath }

// WriteFile writes data to a file within the workspace.
func (w *LocalWorkspace) WriteFile(ctx context.Context, path string, data []byte) error {
	resolved, err := w.resolve(path)
	if err != nil {
		return err
	}

	dir := filepath.Dir(resolved)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("workspace: create parent dir: %w", err)
	}

	return fsutil.WriteFileAtomic(resolved, data, 0o644)
}

// ReadFile reads a file from the workspace.
func (w *LocalWorkspace) ReadFile(ctx context.Context, path string) ([]byte, error) {
	resolved, err := w.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(resolved)
}

// ListFiles lists entries in a directory within the workspace.
func (w *LocalWorkspace) ListFiles(ctx context.Context, dir string) ([]FileInfo, error) {
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
		rel, _ := filepath.Rel(w.basePath, filepath.Join(resolved, e.Name()))
		files = append(files, FileInfo{
			Name:  e.Name(),
			Path:  rel,
			IsDir: e.IsDir(),
			Size:  info.Size(),
		})
	}
	return files, nil
}

// RemoveFile removes a file from the workspace.
func (w *LocalWorkspace) RemoveFile(ctx context.Context, path string) error {
	resolved, err := w.resolve(path)
	if err != nil {
		return err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("workspace: stat: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("workspace: cannot remove directory %q (use RemoveAll)", path)
	}

	return os.Remove(resolved)
}

// Execute runs a shell command with the workspace as the working directory.
func (w *LocalWorkspace) Execute(ctx context.Context, command string) (*ExecResult, error) {
	ctx, cancel := context.WithTimeout(ctx, w.commandTimeout)
	defer cancel()

	cmd := platform.Command(ctx, command)
	cmd.Dir = w.basePath

	var stdout, stderr strings.Builder
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
			return nil, fmt.Errorf("workspace: exec: %w", err)
		}
	}

	return result, nil
}

// OffloadContent writes text content to a file in the workspace, implementing
// the Offloader interface.
func (w *LocalWorkspace) OffloadContent(ctx context.Context, content string, filename string) (string, error) {
	if filename == "" {
		filename = fmt.Sprintf("offload_%d.txt", time.Now().UnixNano())
	}
	path := filepath.Join("_offloaded", filename)
	if err := w.WriteFile(ctx, path, []byte(content)); err != nil {
		return "", err
	}
	return path, nil
}

// OffloadToolResult writes a tool result to a file in the workspace, using
// the tool call ID to derive a unique filename.
func (w *LocalWorkspace) OffloadToolResult(ctx context.Context, content string, toolCallID string) (string, error) {
	filename := fmt.Sprintf("tool_result_%s.txt", toolCallID)
	return w.OffloadContent(ctx, content, filename)
}

// resolve converts a relative path to an absolute path under the workspace,
// rejecting any traversal outside the base directory.
func (w *LocalWorkspace) resolve(path string) (string, error) {
	base := filepath.Clean(w.basePath)
	var cleaned string
	if filepath.IsAbs(path) {
		cleaned = filepath.Clean(path)
	} else {
		cleaned = filepath.Join(base, path)
	}
	// Separator-aware containment: a bare prefix match would admit sibling
	// directories (base /tmp/ws1 must not admit /tmp/ws123/...).
	if cleaned != base && !strings.HasPrefix(cleaned, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, path)
	}
	// Symlink containment: a lexically contained path can still point
	// outside through a link planted inside the workspace (e.g. by an
	// agent's shell). Resolve links — including the base itself, which on
	// some systems is a symlink — and re-check.
	baseResolved := base
	if r, err := filepath.EvalSymlinks(base); err == nil {
		baseResolved = r
	}
	resolved := evalSymlinksLenient(cleaned)
	if resolved != baseResolved && !strings.HasPrefix(resolved, baseResolved+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w via symlink: %q", ErrPathEscape, path)
	}
	return cleaned, nil
}

// evalSymlinksLenient resolves symlinks for p if it exists, otherwise
// resolves the longest existing ancestor and rejoins the non-existent
// remainder (so creating a new file cannot escape via a symlinked parent).
func evalSymlinksLenient(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	dir := p
	var rest []string
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		rest = append([]string{filepath.Base(dir)}, rest...)
		dir = parent
		if _, err := os.Stat(dir); err == nil {
			if resolved, err := filepath.EvalSymlinks(dir); err == nil {
				return filepath.Join(append([]string{resolved}, rest...)...)
			}
			break
		}
	}
	return p
}

type workspaceContextKey struct{}

// WithWorkspace attaches a Workspace to a Go context.
func WithWorkspace(ctx context.Context, ws Workspace) context.Context {
	return context.WithValue(ctx, workspaceContextKey{}, ws)
}

// GetWorkspace retrieves the Workspace from a Go context, or nil.
func GetWorkspace(ctx context.Context) Workspace {
	v, _ := ctx.Value(workspaceContextKey{}).(Workspace)
	return v
}

// Compile-time interface checks.
var _ Workspace = (*LocalWorkspace)(nil)
var _ Offloader = (*LocalWorkspace)(nil)
