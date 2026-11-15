package tool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// recordingBackend is a tool.Backend that records the last ExecShell command and
// returns a fixed marker, to prove bash execution is diverted into it.
type recordingBackend struct {
	lastCmd string
}

func (b *recordingBackend) ExecShell(_ context.Context, command string, _ time.Duration) (*ExecResult, error) {
	b.lastCmd = command
	return &ExecResult{Stdout: "FROM_BACKEND:" + command}, nil
}
func (b *recordingBackend) ReadFile(context.Context, string) ([]byte, error)  { return nil, nil }
func (b *recordingBackend) WriteFile(context.Context, string, []byte) error   { return nil }
func (b *recordingBackend) FileExists(context.Context, string) (bool, error)  { return false, nil }
func (b *recordingBackend) ListDir(context.Context, string) ([]string, error) { return nil, nil }
func (b *recordingBackend) Glob(context.Context, string) ([]string, error)    { return nil, nil }

// TestBashTool_RoutesThroughConfiguredBackend proves that when a custom backend
// is attached to the context, bash runs the command inside it (isolation) rather
// than on the host.
func TestBashTool_RoutesThroughConfiguredBackend(t *testing.T) {
	be := &recordingBackend{}
	ctx := WithBackend(context.Background(), be)

	resp, err := BashTool().Execute(ctx, map[string]any{"command": "echo hello"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if be.lastCmd != "echo hello" {
		t.Errorf("backend received %q, want %q", be.lastCmd, "echo hello")
	}
	var text string
	for _, blk := range resp.Content {
		if tb, ok := blk.(message.TextBlock); ok {
			text += tb.Text
		}
	}
	if !strings.Contains(text, "FROM_BACKEND:echo hello") {
		t.Errorf("expected backend output, got %q", text)
	}
}
