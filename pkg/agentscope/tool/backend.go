package tool

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/platform"
)

// Backend abstracts the execution environment for tool operations, allowing
// tools to operate on local file systems, remote containers, or other
// sandboxed environments.
type Backend interface {
	// ExecShell runs a shell command and returns the result.
	ExecShell(ctx context.Context, command string, timeout time.Duration) (*ExecResult, error)

	// ReadFile reads a file and returns its contents.
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// WriteFile writes data to a file, creating parent directories as needed.
	WriteFile(ctx context.Context, path string, data []byte) error

	// FileExists reports whether the named file or directory exists.
	FileExists(ctx context.Context, path string) (bool, error)

	// ListDir returns the names of entries in the given directory.
	ListDir(ctx context.Context, path string) ([]string, error)

	// Glob returns the names of files matching the pattern.
	Glob(ctx context.Context, pattern string) ([]string, error)
}

// ExecResult holds the outcome of a shell command execution.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// LocalBackend is a Backend implementation that operates on the local
// file system and runs shell commands via /bin/sh.
type LocalBackend struct {
	// WorkDir is the working directory for shell commands.
	// If empty, the current process working directory is used.
	WorkDir string
}

// ExecShell runs a shell command locally.
func (b *LocalBackend) ExecShell(ctx context.Context, command string, timeout time.Duration) (*ExecResult, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := platform.Command(ctx, command)
	if b.WorkDir != "" {
		cmd.Dir = b.WorkDir
	}

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
			return nil, fmt.Errorf("backend: exec: %w", err)
		}
	}

	return result, nil
}

// ReadFile reads a file from the local file system.
func (b *LocalBackend) ReadFile(_ context.Context, path string) ([]byte, error) {
	return os.ReadFile(path)
}

// WriteFile writes data to a file on the local file system, creating parent
// directories as needed.
func (b *LocalBackend) WriteFile(_ context.Context, path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("backend: create dir: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// FileExists checks whether a file or directory exists on the local file system.
func (b *LocalBackend) FileExists(_ context.Context, path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// ListDir returns the entries in a directory on the local file system.
func (b *LocalBackend) ListDir(_ context.Context, path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

// Glob returns file names matching a pattern on the local file system.
func (b *LocalBackend) Glob(_ context.Context, pattern string) ([]string, error) {
	return filepath.Glob(pattern)
}

// backendContextKey is the context key for Backend values.
type backendContextKey struct{}

// WithBackend attaches a Backend to a Go context.
func WithBackend(ctx context.Context, backend Backend) context.Context {
	return context.WithValue(ctx, backendContextKey{}, backend)
}

// GetBackend retrieves the Backend from a Go context.
// Returns a default LocalBackend if none is set.
func GetBackend(ctx context.Context) Backend {
	if b, ok := ctx.Value(backendContextKey{}).(Backend); ok {
		return b
	}
	return &LocalBackend{}
}

// getBackendIfSet returns the Backend explicitly attached to ctx, if any. Unlike
// GetBackend it does not substitute a default, so callers can tell whether a
// custom (e.g. Docker/E2B) backend was configured and only then divert execution
// into it — preserving the rich local path when none is set.
func getBackendIfSet(ctx context.Context) (Backend, bool) {
	b, ok := ctx.Value(backendContextKey{}).(Backend)
	return b, ok
}

// Compile-time interface check.
var _ Backend = (*LocalBackend)(nil)

// BackendPermissionContext derives a per-call permission context override
// when a workspace backend will execute commands: every non-local backend
// routes into a POSIX environment (Docker/K8s/Bubblewrap/E2B/Daytona/
// AppleContainer/OpenSandbox), so host-based shell detection must not drive
// shell-specific permission checks (residual of upstream #2366). Returns nil
// when no override applies: no backend in ctx, host-local backend, nil base
// context, or an explicit TargetShell already pinned by configuration.
func BackendPermissionContext(ctx context.Context, base *permission.Context) *permission.Context {
	b, ok := getBackendIfSet(ctx)
	if !ok {
		return nil
	}
	if _, isLocal := b.(*LocalBackend); isLocal {
		return nil
	}
	if base == nil || base.TargetShell != "" {
		return nil
	}
	override := *base
	override.TargetShell = "posix"
	return &override
}

// BackendFileInfo describes a file as seen by the backend's own filesystem.
type BackendFileInfo struct {
	ModTime time.Time
	Size    int64
}

// BackendStatter is an optional Backend capability. Backends whose files
// live outside the host filesystem (Docker/K8s/E2B/...) implement it so the
// read cache can validate freshness against the SAME filesystem that served
// the read (upstream #2092). Consumers must type-assert; a backend without
// StatFile simply provides no backend-side freshness data.
type BackendStatter interface {
	StatFile(ctx context.Context, path string) (BackendFileInfo, error)
}

// StatFile implements BackendStatter on the local filesystem.
func (b *LocalBackend) StatFile(_ context.Context, path string) (BackendFileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return BackendFileInfo{}, err
	}
	return BackendFileInfo{ModTime: info.ModTime(), Size: info.Size()}, nil
}

// backendMtime stats path through the backend when it implements
// BackendStatter, so cache freshness is judged by the filesystem that served
// the read (upstream #2092). Returns nil when unavailable, which makes the
// ReadCache fall back to its host-stat behavior.
func backendMtime(ctx context.Context, b Backend, p string) *time.Time {
	st, ok := b.(BackendStatter)
	if !ok {
		return nil
	}
	info, err := st.StatFile(ctx, p)
	if err != nil {
		return nil
	}
	mt := info.ModTime
	return &mt
}
