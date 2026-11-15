package workspace

import (
	"context"
	"fmt"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// ToolBackend adapts a Workspace to tool.Backend so the file/shell builtin tools
// (read/write/edit/bash/grep/glob) operate inside the workspace — a Docker or
// E2B sandbox — instead of on the host. This gives real isolation.
//
// Wire it into tool execution via:
//
//	ctx = tool.WithBackend(ctx, workspace.NewToolBackend(ws))
type ToolBackend struct {
	ws Workspace
}

// NewToolBackend returns a tool.Backend backed by the given Workspace.
func NewToolBackend(ws Workspace) *ToolBackend { return &ToolBackend{ws: ws} }

// ExecShell runs a command inside the workspace. The timeout is governed by the
// workspace's own execution policy.
func (b *ToolBackend) ExecShell(ctx context.Context, command string, _ time.Duration) (*tool.ExecResult, error) {
	res, err := b.ws.Execute(ctx, command)
	if err != nil {
		return nil, err
	}
	return &tool.ExecResult{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode}, nil
}

// ReadFile reads a file from the workspace.
func (b *ToolBackend) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return b.ws.ReadFile(ctx, path)
}

// WriteFile writes a file into the workspace.
func (b *ToolBackend) WriteFile(ctx context.Context, path string, data []byte) error {
	return b.ws.WriteFile(ctx, path, data)
}

// FileExists reports whether a file exists in the workspace.
func (b *ToolBackend) FileExists(ctx context.Context, path string) (bool, error) {
	if _, err := b.ws.ReadFile(ctx, path); err == nil {
		return true, nil
	}
	return false, nil
}

// ListDir returns the entry names of a directory in the workspace.
func (b *ToolBackend) ListDir(ctx context.Context, path string) ([]string, error) {
	fis, err := b.ws.ListFiles(ctx, path)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(fis))
	for i, fi := range fis {
		names[i] = fi.Name
	}
	return names, nil
}

// Glob matches a single-level pattern within the workspace. It lists the
// pattern's directory and applies filepath.Match to each entry — no shell is
// invoked, so it is injection-safe (recursive ** is not supported).
func (b *ToolBackend) Glob(ctx context.Context, pattern string) ([]string, error) {
	dir := filepath.Dir(pattern)
	base := filepath.Base(pattern)
	fis, err := b.ws.ListFiles(ctx, dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, fi := range fis {
		if ok, _ := filepath.Match(base, fi.Name); ok {
			if dir == "." {
				out = append(out, fi.Name)
			} else {
				out = append(out, filepath.Join(dir, fi.Name))
			}
		}
	}
	return out, nil
}

// Compile-time check.
var _ tool.Backend = (*ToolBackend)(nil)

// StatFile implements tool.BackendStatter: it reports file metadata from the
// workspace's OWN filesystem via a POSIX stat call, so the tool read cache
// validates freshness against the same filesystem that served the read
// (upstream #2092). Both GNU (`stat -c %Y`) and BSD/busybox (`stat -f %m`)
// flag forms are attempted. Granularity is one second — sub-second
// write/read races can theoretically pass a stale entry; Write/Edit
// invalidate their cache entries explicitly, which covers the practical
// Read->Edit flow.
func (b *ToolBackend) StatFile(ctx context.Context, p string) (tool.BackendFileInfo, error) {
	cleaned, err := b.jailPath(p)
	if err != nil {
		return tool.BackendFileInfo{}, err
	}
	// The spelling used inside the shell must name the SAME file ReadFile read,
	// and that is backend-specific (see ExecPathResolver): Docker/Daytona/
	// AppleContainer resolve to an absolute in-sandbox path, while K8s, E2B,
	// OpenSandbox and bubblewrap need the caller-relative form. Getting this
	// wrong is worse than a wasted exec — a stat that lands on a different file
	// supplies a freshness key for the wrong file and the read cache can then
	// serve stale content as fresh.
	execPath := cleaned
	if r, ok := b.ws.(ExecPathResolver); ok {
		execPath = r.ExecPath(cleaned)
	}
	// POSIX single-quote escaping: close the quote, emit an escaped quote,
	// reopen. Inside single quotes the shell gives no special meaning to
	// backticks, $, spaces or glob characters.
	quoted := "'" + strings.ReplaceAll(execPath, "'", `'\''`) + "'"
	cmd := "stat -c %Y " + quoted + " 2>/dev/null || stat -f %m " + quoted
	res, err := b.ws.Execute(ctx, cmd)
	if err != nil {
		return tool.BackendFileInfo{}, err
	}
	if res.ExitCode != 0 {
		return tool.BackendFileInfo{}, fmt.Errorf("workspace stat %s: exit %d: %s", p, res.ExitCode, res.Stderr)
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
	if err != nil {
		return tool.BackendFileInfo{}, fmt.Errorf("workspace stat %s: parse %q: %w", p, res.Stdout, err)
	}
	return tool.BackendFileInfo{ModTime: time.Unix(secs, 0)}, nil
}

// jailPath applies a lexical containment check before StatFile interpolates a
// path into a shell command.
//
// ReadFile/WriteFile go through the workspace's own resolver, which also
// follows symlinks; StatFile has no such call to lean on, so without this it
// would be an oracle for the mtime and existence of ANY path the execution
// user can reach — harmless while it is only called after a successful
// ReadFile (as the read cache does), but a live information leak the moment
// someone wires it earlier.
//
// Symlinks are deliberately not resolved here: doing so would need a second
// exec round-trip on every cache probe. The check is containment of the
// requested path, and the underlying ReadFile remains the authoritative jail.
func (b *ToolBackend) jailPath(p string) (string, error) {
	cleaned := pathpkg.Clean(strings.TrimSpace(filepath.ToSlash(p)))
	if cleaned == "" || cleaned == "." {
		return "", fmt.Errorf("workspace stat: empty path")
	}
	escapes := cleaned == ".." || strings.HasPrefix(cleaned, "../")

	base := pathpkg.Clean(filepath.ToSlash(b.ws.BasePath()))
	if base == "" || base == "." {
		// The workspace exposes no base to confine against (a custom
		// implementation). Only the parent-relative escape can be rejected
		// from here; the sandbox itself is the boundary.
		if escapes {
			return "", fmt.Errorf("%w: %q", ErrPathEscape, p)
		}
		return cleaned, nil
	}

	abs := cleaned
	if !pathpkg.IsAbs(abs) {
		abs = pathpkg.Join(base, cleaned)
	}
	// Separator-aware: a bare prefix match would admit sibling directories
	// (base /tmp/ws1 must not admit /tmp/ws123/secret). A base of "/" contains
	// everything by definition.
	if base != "/" && abs != base && !strings.HasPrefix(abs, base+"/") {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, p)
	}
	// Containment is judged on the absolute form, but the caller-relative form
	// is what gets returned: whether the shell needs a relative or an absolute
	// spelling is backend-specific and is decided by StatFile through
	// ExecPathResolver. Returning abs unconditionally would be wrong for every
	// backend whose Execute passes the caller's path straight through (K8s,
	// E2B, OpenSandbox) and for bubblewrap, whose BasePath is a HOST path while
	// the sandbox sees that directory as "/".
	return cleaned, nil
}
