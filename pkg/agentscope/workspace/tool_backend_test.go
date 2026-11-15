package workspace

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// TestToolBackend_AdaptsWorkspace verifies the adapter routes tool.Backend
// operations through a Workspace, so file/shell builtin tools run inside the
// workspace (Docker/E2B) rather than on the host.
func TestToolBackend_AdaptsWorkspace(t *testing.T) {
	ws, err := NewLocalWorkspace(LocalConfig{BasePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var b tool.Backend = NewToolBackend(ws)
	ctx := context.Background()

	if err := b.WriteFile(ctx, "sub/a.txt", []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := b.ReadFile(ctx, "sub/a.txt")
	if err != nil || string(data) != "hello" {
		t.Fatalf("read = %q, %v; want hello", data, err)
	}
	if ok, _ := b.FileExists(ctx, "sub/a.txt"); !ok {
		t.Error("FileExists should be true for a written file")
	}
	if ok, _ := b.FileExists(ctx, "sub/missing.txt"); ok {
		t.Error("FileExists should be false for a missing file")
	}

	names, err := b.ListDir(ctx, "sub")
	if err != nil || len(names) != 1 || names[0] != "a.txt" {
		t.Fatalf("ListDir = %v, %v; want [a.txt]", names, err)
	}

	matches, err := b.Glob(ctx, "sub/*.txt")
	if err != nil || len(matches) != 1 {
		t.Fatalf("Glob = %v, %v; want 1 match", matches, err)
	}

	res, err := b.ExecShell(ctx, "echo hi", 0)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.Stdout == "" {
		t.Error("ExecShell produced no stdout")
	}
}
