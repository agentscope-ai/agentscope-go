package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestMultiEdit_AppliesAllAtomically(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.go")
	if err := os.WriteFile(p, []byte("alpha beta gamma"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Mark as read so the read-before-write guard (if any) passes; no ReadCache
	// is set on this ctx, so the guard is inactive anyway.
	ctx := context.Background()

	_, err := MultiEditTool().Execute(ctx, map[string]any{
		"file_path": p,
		"edits": []any{
			map[string]any{"old_string": "alpha", "new_string": "A"},
			map[string]any{"old_string": "gamma", "new_string": "G"},
		},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "A beta G" {
		t.Fatalf("got %q, want %q", got, "A beta G")
	}
}

func TestMultiEdit_AtomicOnFailure(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	original := "one two three"
	if err := os.WriteFile(p, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	// Second edit's old_string is absent -> the whole operation must fail and
	// leave the file unchanged.
	resp, err := MultiEditTool().Execute(context.Background(), map[string]any{
		"file_path": p,
		"edits": []any{
			map[string]any{"old_string": "one", "new_string": "1"},
			map[string]any{"old_string": "MISSING", "new_string": "x"},
		},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Errorf("expected error state for a failing edit, got %q", resp.State)
	}
	got, _ := os.ReadFile(p)
	if string(got) != original {
		t.Fatalf("file was modified despite failed edit: %q", got)
	}
}
