package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestResolvePath_JailConfinesToRoot(t *testing.T) {
	root := t.TempDir()
	ctx := WithWorkspaceRoot(context.Background(), root)

	// A path inside the workspace resolves fine.
	if _, err := resolvePath(ctx, filepath.Join(root, "sub", "file.txt")); err != nil {
		t.Errorf("inside-root path was rejected: %v", err)
	}

	// Traversal escapes are rejected.
	for _, p := range []string{
		filepath.Join(root, "..", "escape.txt"),
		"/etc/passwd",
		"/root/.ssh/id_rsa",
	} {
		if _, err := resolvePath(ctx, p); err == nil {
			t.Errorf("expected escape %q to be rejected", p)
		}
	}
}

func TestResolvePath_NoRootIsUnconfined(t *testing.T) {
	// With no workspace root configured, behavior is unchanged (absolute paths
	// resolve without confinement) so existing callers are unaffected.
	if _, err := resolvePath(context.Background(), "/etc/hosts"); err != nil {
		t.Errorf("unconfined resolve should succeed, got %v", err)
	}
}

// TestReadTool_RespectsWorkspaceJail is the end-to-end guard: the read tool
// cannot read outside the configured workspace root.
func TestReadTool_RespectsWorkspaceJail(t *testing.T) {
	root := t.TempDir()
	ctx := WithWorkspaceRoot(context.Background(), root)

	rt := ReadTool()
	resp, err := rt.Execute(ctx, map[string]any{"file_path": "/etc/passwd"})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("expected read of /etc/passwd to be rejected under workspace jail, got state %q", resp.State)
	}
}

func TestResolvePath_EscapeErrorIsTyped(t *testing.T) {
	root := t.TempDir()
	ctx := WithWorkspaceRoot(context.Background(), root)

	_, err := resolvePath(ctx, "/etc/passwd")
	if err == nil {
		t.Fatal("expected escape to be rejected")
	}
	esc, ok := err.(*WorkspaceEscapeError)
	if !ok {
		t.Fatalf("expected *WorkspaceEscapeError, got %T: %v", err, err)
	}
	if esc.Path != "/etc/passwd" || esc.Root != root {
		t.Errorf("escape error carries wrong fields: %+v", esc)
	}
}

func TestResolvePath_GrantsWidenJail(t *testing.T) {
	root := t.TempDir()
	granted := t.TempDir()
	other := t.TempDir()

	ctx := WithWorkspaceGrants(WithWorkspaceRoot(context.Background(), root), []string{granted})

	// Granted directory and its children resolve.
	if _, err := resolvePath(ctx, filepath.Join(granted, "f.txt")); err != nil {
		t.Errorf("granted dir was rejected: %v", err)
	}
	// The workspace root still works.
	if _, err := resolvePath(ctx, filepath.Join(root, "f.txt")); err != nil {
		t.Errorf("workspace root was rejected: %v", err)
	}
	// Ungranted directories still escape.
	if _, err := resolvePath(ctx, filepath.Join(other, "f.txt")); err == nil {
		t.Error("ungranted dir should still be rejected")
	}
	// A grant does not leak sideways: a sibling with a shared name prefix
	// ("/tmp/grantedx") must not be covered by a grant on "/tmp/granted".
	if _, err := resolvePath(ctx, granted+"x"); err == nil {
		t.Error("prefix-sibling of a grant should still be rejected")
	}
}

func TestResolvePath_GrantSymlinkResolved(t *testing.T) {
	root := t.TempDir()
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	ctx := WithWorkspaceGrants(WithWorkspaceRoot(context.Background(), root), []string{link})
	// Reaching the real directory through its resolved path is allowed via
	// the symlinked grant.
	if _, err := resolvePath(ctx, filepath.Join(real, "f.txt")); err != nil {
		t.Errorf("resolved target of symlinked grant was rejected: %v", err)
	}
	// And through the link itself.
	if _, err := resolvePath(ctx, filepath.Join(link, "f.txt")); err != nil {
		t.Errorf("symlinked grant path was rejected: %v", err)
	}
}

func TestResolvePath_GrantsIgnoredWithoutRoot(t *testing.T) {
	// Without a root there is no jail to widen; grants must not change the
	// unconfined behavior.
	ctx := WithWorkspaceGrants(context.Background(), []string{t.TempDir()})
	if _, err := resolvePath(ctx, "/etc/hosts"); err != nil {
		t.Errorf("unconfined resolve should succeed, got %v", err)
	}
}

func TestNewErrorResponse_EscapeMetadata(t *testing.T) {
	esc := &WorkspaceEscapeError{Path: "/x", Root: "/r"}
	resp := NewErrorResponse(fmt.Errorf("invalid path: %w", esc))
	info, ok := resp.Metadata[WorkspaceEscapeMetaKey].(WorkspaceEscapeInfo)
	if !ok {
		t.Fatalf("escape metadata missing: %+v", resp.Metadata)
	}
	if info.Path != "/x" || info.Root != "/r" {
		t.Errorf("wrong escape info: %+v", info)
	}

	plain := NewErrorResponse(fmt.Errorf("some other error"))
	if plain.Metadata != nil {
		t.Errorf("non-escape error must not carry metadata, got %+v", plain.Metadata)
	}
}
