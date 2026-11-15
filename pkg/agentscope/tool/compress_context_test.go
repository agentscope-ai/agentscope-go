package tool

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

func compressText(t *testing.T, resp *ToolResponse) string {
	t.Helper()
	if len(resp.Content) == 0 {
		t.Fatal("empty response content")
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("content[0] = %T, want TextBlock", resp.Content[0])
	}
	return tb.Text
}

// Upstream #2143: agent-driven context compression tool.
func TestCompressContextToolSuccess(t *testing.T) {
	called := false
	tk := NewCompressContextTool(func(_ context.Context) (CompressionResult, error) {
		called = true
		return CompressionResult{Compressed: true}, nil
	})
	if tk.Name() != "compress_context" {
		t.Errorf("name = %q", tk.Name())
	}
	if len(tk.InputSchema()) == 0 {
		t.Error("schema missing")
	}
	resp, err := tk.Execute(context.Background(), map[string]any{"reason": "history too long"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !called {
		t.Error("compress func not invoked")
	}
	if resp.State == message.ToolResultError {
		t.Errorf("state = %v, want success", resp.State)
	}
	if !strings.Contains(compressText(t, resp), "Context compressed") {
		t.Errorf("response text = %v", resp.Content)
	}
}

// The tool must not claim success when nothing was compressed. Telling the
// model "older messages were summarized" while the context is unchanged makes
// it believe details are still reachable that it can no longer see.
func TestCompressContextToolReportsNoopHonestly(t *testing.T) {
	tk := NewCompressContextTool(func(_ context.Context) (CompressionResult, error) {
		return CompressionResult{Compressed: false}, nil
	})
	resp, err := tk.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.State == message.ToolResultError {
		t.Errorf("state = %v; a no-op is not a failure", resp.State)
	}
	text := compressText(t, resp)
	if strings.Contains(text, "Context compressed") {
		t.Errorf("no-op must not report success, got %q", text)
	}
	if !strings.Contains(text, "No compression was needed") {
		t.Errorf("no-op text = %q", text)
	}
	if !strings.Contains(text, "nothing was summarized") {
		t.Errorf("no-op text should say nothing changed, got %q", text)
	}
}

// A caller-supplied detail is surfaced either way.
func TestCompressContextToolIncludesDetail(t *testing.T) {
	tk := NewCompressContextTool(func(_ context.Context) (CompressionResult, error) {
		return CompressionResult{Compressed: true, Detail: "(12000 -> 3400 tokens)"}, nil
	})
	resp, err := tk.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(compressText(t, resp), "12000 -> 3400") {
		t.Errorf("detail missing from %q", compressText(t, resp))
	}
}

func TestCompressContextToolError(t *testing.T) {
	tk := NewCompressContextTool(func(_ context.Context) (CompressionResult, error) {
		return CompressionResult{}, errors.New("summary generation failed")
	})
	resp, err := tk.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Errorf("state = %v, want error", resp.State)
	}
}

func TestCompressContextToolNilFuncPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("nil compress func must panic (construction contract)")
		}
	}()
	NewCompressContextTool(nil)
}

// The tool rewrites the agent's message history, so it must never run inside a
// parallel tool batch (upstream marks it is_concurrency_safe=False).
func TestCompressContextToolIsNotConcurrencySafe(t *testing.T) {
	tk := NewCompressContextTool(func(_ context.Context) (CompressionResult, error) {
		return CompressionResult{}, nil
	})
	if tk.IsConcurrencySafe() {
		t.Error("compress_context must not be concurrency safe")
	}
}

// Compression is internal maintenance on the agent's own context, not an
// action on the user's environment: gating it behind a confirmation would
// stall every model-initiated compression.
func TestCompressContextToolPermissionIsAllow(t *testing.T) {
	tk := NewCompressContextTool(func(_ context.Context) (CompressionResult, error) {
		return CompressionResult{}, nil
	})
	ct, ok := tk.(interface {
		CheckPermissions(map[string]any, *permission.Context) permission.Decision
	})
	if !ok {
		t.Fatal("tool does not implement CheckPermissions")
	}
	d := ct.CheckPermissions(nil, &permission.Context{})
	if d.Behavior != permission.BehaviorAllow {
		t.Errorf("behavior = %v, want allow", d.Behavior)
	}
}
