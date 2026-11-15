package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// workspaceRootKey is the context key carrying the workspace-root jail.
type workspaceRootKey struct{}

// workspaceGrantsKey is the context key carrying session-granted directories
// that widen the workspace-root jail.
type workspaceGrantsKey struct{}

// WithWorkspaceRoot confines file-tool path resolution to root. Any path that
// resolves (symlinks included) outside root is rejected unless covered by a
// WithWorkspaceGrants directory. When no root is set, path resolution is
// unconfined (backward-compatible default) and grants are irrelevant.
func WithWorkspaceRoot(ctx context.Context, root string) context.Context {
	return context.WithValue(ctx, workspaceRootKey{}, root)
}

// getWorkspaceRoot returns the configured workspace root, or "" if unset.
func getWorkspaceRoot(ctx context.Context) string {
	if r, ok := ctx.Value(workspaceRootKey{}).(string); ok {
		return r
	}
	return ""
}

// WithWorkspaceGrants widens the workspace-root jail to additionally cover
// dirs. Grants model user-approved access beyond the workspace (e.g. an
// escape the user confirmed in session); they never narrow the jail and are
// ignored when no root is set. The slice is copied.
func WithWorkspaceGrants(ctx context.Context, dirs []string) context.Context {
	if len(dirs) == 0 {
		return ctx
	}
	cp := append([]string(nil), dirs...)
	return context.WithValue(ctx, workspaceGrantsKey{}, cp)
}

// getWorkspaceGrants returns the session-granted directories, or nil.
func getWorkspaceGrants(ctx context.Context) []string {
	if g, ok := ctx.Value(workspaceGrantsKey{}).([]string); ok {
		return g
	}
	return nil
}

// WorkspaceEscapeMetaKey marks a tool error response whose cause is the
// workspace jail rejecting a path. NewErrorResponse attaches it (as a
// WorkspaceEscapeInfo value) when the error chain holds a
// *WorkspaceEscapeError, letting the agent layer offer a grant prompt
// instead of surfacing a dead-end error.
const WorkspaceEscapeMetaKey = "workspace_escape"

// WorkspaceEscapeInfo describes a jail escape for ToolResponse.Metadata.
type WorkspaceEscapeInfo struct {
	// Path is the offending path as supplied in the tool input.
	Path string `json:"path"`
	// Root is the active workspace root the path escaped.
	Root string `json:"root"`
}

// WorkspaceEscapeError is returned by resolvePath when a jailed path escapes
// the workspace root and no session grant covers it.
type WorkspaceEscapeError struct {
	Path string
	Root string
}

// Error implements error.
func (e *WorkspaceEscapeError) Error() string {
	return fmt.Sprintf("path %q escapes workspace root %q", e.Path, e.Root)
}

// resolvePath resolves path to an absolute, cleaned path. If a workspace root is
// configured on ctx, the result is confined to that root (symlink-aware), and an
// error is returned for any path that escapes it. This is the single choke point
// file tools use so an agent cannot read /etc/shadow or write outside its jail.
func resolvePath(ctx context.Context, path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}

	root := getWorkspaceRoot(ctx)
	if root == "" {
		return abs, nil
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("invalid workspace root: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = resolved
	}

	// Resolve symlinks on the target (or its nearest existing ancestor, so that
	// writing a new file cannot escape via a symlinked parent directory).
	target := evalSymlinksLenient(abs)

	if !withinRoot(rootAbs, target) {
		// Session grants widen the jail: each granted directory is resolved
		// like the root and must contain the target.
		for _, grant := range getWorkspaceGrants(ctx) {
			grantAbs, err := filepath.Abs(grant)
			if err != nil {
				continue
			}
			if resolved, err := filepath.EvalSymlinks(grantAbs); err == nil {
				grantAbs = resolved
			}
			if withinRoot(grantAbs, target) {
				return abs, nil
			}
		}
		return "", &WorkspaceEscapeError{Path: path, Root: root}
	}
	return abs, nil
}

// evalSymlinksLenient resolves symlinks for p if it exists, otherwise resolves
// the longest existing ancestor and rejoins the non-existent remainder.
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

// withinRoot reports whether target is root itself or nested under it, using a
// separator-safe relative-path check (so "/rootx" is not treated as under "/root").
func withinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
