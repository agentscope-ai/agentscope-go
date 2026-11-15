package tool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

var errMemNotExist = errors.New("file does not exist")

// memBackend is an in-memory tool.Backend for testing file-tool routing.
type memBackend struct{ files map[string][]byte }

func newMemBackend() *memBackend { return &memBackend{files: map[string][]byte{}} }

func (m *memBackend) ExecShell(context.Context, string, time.Duration) (*ExecResult, error) {
	return &ExecResult{}, nil
}
func (m *memBackend) ReadFile(_ context.Context, path string) ([]byte, error) {
	d, ok := m.files[path]
	if !ok {
		return nil, errMemNotExist
	}
	return d, nil
}
func (m *memBackend) WriteFile(_ context.Context, path string, data []byte) error {
	m.files[path] = append([]byte(nil), data...)
	return nil
}
func (m *memBackend) FileExists(_ context.Context, path string) (bool, error) {
	_, ok := m.files[path]
	return ok, nil
}
func (m *memBackend) ListDir(context.Context, string) ([]string, error) { return nil, nil }
func (m *memBackend) Glob(context.Context, string) ([]string, error)    { return nil, nil }

func textOf(resp *ToolResponse) string {
	var b strings.Builder
	for _, blk := range resp.Content {
		if tb, ok := blk.(message.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// TestFileTools_RouteThroughBackend proves write/read/edit all operate on the
// configured backend (Docker/E2B) using workspace-relative paths, not the host.
func TestFileTools_RouteThroughBackend(t *testing.T) {
	be := newMemBackend()
	ctx := WithBackend(context.Background(), be)

	// Write.
	if _, err := WriteTool().Execute(ctx, map[string]any{"file_path": "src/a.txt", "content": "hello\nworld\n"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := string(be.files["src/a.txt"]); got != "hello\nworld\n" {
		t.Fatalf("backend file = %q; want hello/world", got)
	}

	// Read.
	resp, err := ReadTool().Execute(ctx, map[string]any{"file_path": "src/a.txt"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := textOf(resp)
	if !strings.Contains(out, "1\thello") || !strings.Contains(out, "2\tworld") {
		t.Fatalf("read output missing numbered lines: %q", out)
	}

	// Edit.
	if _, err := EditTool().Execute(ctx, map[string]any{"file_path": "src/a.txt", "old_string": "world", "new_string": "gopher"}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got := string(be.files["src/a.txt"]); got != "hello\ngopher\n" {
		t.Fatalf("after edit = %q; want hello/gopher", got)
	}
}
