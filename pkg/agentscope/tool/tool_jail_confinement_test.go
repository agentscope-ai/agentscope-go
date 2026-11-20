package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestViewTextFileTool_RespectsWorkspaceJail(t *testing.T) {
	root := t.TempDir()
	ctx := WithWorkspaceRoot(context.Background(), root)
	resp, err := ViewTextFileTool().Execute(ctx, map[string]any{"path": "/etc/passwd"})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("view_text_file outside jail must be rejected, got %q", resp.State)
	}
}

func TestGlobTool_RespectsWorkspaceJail(t *testing.T) {
	root := t.TempDir()
	ctx := WithWorkspaceRoot(context.Background(), root)
	// base_dir outside the jail is rejected.
	resp, err := GlobTool().Execute(ctx, map[string]any{"pattern": "*", "base_dir": "/etc"})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("Glob with outside base_dir must be rejected, got %q", resp.State)
	}
	// A traversing pattern must not list paths outside the jail.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, outside)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = GlobTool().Execute(ctx, map[string]any{"pattern": rel + "/*", "base_dir": root})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	var text string
	for _, b := range resp.Content {
		if tb, ok := b.(message.TextBlock); ok {
			text += tb.Text
		}
	}
	if strings.Contains(text, "secret.txt") {
		t.Fatalf("Glob leaked an outside path: %s", text)
	}
}

func TestLSPTool_RespectsWorkspaceJail(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	ctx := WithWorkspaceRoot(context.Background(), root)
	// A supported-extension file outside the jail must be rejected by the jail
	// itself (not by language detection), before any server is started.
	resp, err := LSPTool().Execute(ctx, map[string]any{"operation": "hover", "filePath": filepath.Join(outside, "file.go")})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("LSP outside jail must be rejected, got %q", resp.State)
	}
	if _, ok := resp.Metadata[WorkspaceEscapeMetaKey]; !ok {
		t.Fatalf("LSP rejection must carry the workspace-escape marker, got metadata %v", resp.Metadata)
	}
}

func TestGlobTool_SymlinkLeakBlocked(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "sub", "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ctx := WithWorkspaceRoot(context.Background(), root)
	for _, pattern := range []string{"link/*", "link/sub/**"} {
		resp, err := GlobTool().Execute(ctx, map[string]any{"pattern": pattern, "base_dir": root})
		if err != nil {
			t.Fatalf("unexpected transport error: %v", err)
		}
		var text string
		for _, b := range resp.Content {
			if tb, ok := b.(message.TextBlock); ok {
				text += tb.Text
			}
		}
		if strings.Contains(text, "secret.txt") {
			t.Fatalf("Glob pattern %q leaked an outside path via symlink: %s", pattern, text)
		}
	}
}
