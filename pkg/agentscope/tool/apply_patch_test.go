package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestApplyUnifiedDiff_ReplaceLine(t *testing.T) {
	orig := "line1\nline2\nline3\n"
	patch := "@@ -1,3 +1,3 @@\n line1\n-line2\n+LINE2\n line3\n"
	got, err := applyUnifiedDiff(orig, patch)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got != "line1\nLINE2\nline3\n" {
		t.Fatalf("got %q", got)
	}
}

func TestApplyUnifiedDiff_ContextMismatch(t *testing.T) {
	orig := "line1\nline2\nline3\n"
	patch := "@@ -1,3 +1,3 @@\n WRONG\n-line2\n+X\n line3\n"
	if _, err := applyUnifiedDiff(orig, patch); err == nil {
		t.Fatal("expected context-mismatch error")
	}
}

func TestApplyUnifiedDiff_MultiHunk(t *testing.T) {
	orig := "a\nb\nc\nd\ne\n"
	patch := "@@ -1,1 +1,1 @@\n-a\n+A\n@@ -5,1 +5,1 @@\n-e\n+E\n"
	got, err := applyUnifiedDiff(orig, patch)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got != "A\nb\nc\nd\nE\n" {
		t.Fatalf("got %q", got)
	}
}

func TestApplyPatchTool_AppliesAndIsAtomicOnFailure(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("x\ny\nz\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Success.
	if _, err := ApplyPatchTool().Execute(context.Background(), map[string]any{
		"file_path": p,
		"patch":     "@@ -1,3 +1,3 @@\n x\n-y\n+Y\n z\n",
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "x\nY\nz\n" {
		t.Fatalf("after apply = %q", b)
	}

	// Failure (bad context) must not modify the file.
	resp, err := ApplyPatchTool().Execute(context.Background(), map[string]any{
		"file_path": p,
		"patch":     "@@ -1,1 +1,1 @@\n-NOPE\n+Q\n",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Errorf("expected error state on bad patch")
	}
	if b, _ := os.ReadFile(p); string(b) != "x\nY\nz\n" {
		t.Fatalf("file changed despite failed patch: %q", b)
	}
}
