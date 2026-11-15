package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestWrapNonStreamingTool(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`)
	ft := NewFunctionTool("test_tool", "test", schema, func(ctx context.Context, input map[string]any) (any, error) {
		return input["x"].(string) + " streamed", nil
	})

	st := WrapNonStreamingTool(ft)
	if !IsStreamingTool(st) {
		t.Fatal("WrapNonStreamingTool should return a StreamingTool")
	}

	ch, err := st.ExecuteStream(context.Background(), map[string]any{"x": "hello"})
	if err != nil {
		t.Fatal(err)
	}

	var chunks []ToolChunk
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if !chunks[0].IsFinal {
		t.Fatal("expected final chunk")
	}
	if chunks[0].State != message.ToolResultSuccess {
		t.Fatalf("state = %s, want success", chunks[0].State)
	}

	text := ""
	for _, b := range chunks[0].Content {
		if tb, ok := b.(message.TextBlock); ok {
			text = tb.Text
		}
	}
	if text != "hello streamed" {
		t.Fatalf("text = %q, want %q", text, "hello streamed")
	}
}

func TestCollectStream(t *testing.T) {
	ch := make(chan ToolChunk, 3)
	ch <- ToolChunk{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "part1"}},
	}
	ch <- ToolChunk{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "part2"}},
	}
	ch <- ToolChunk{
		Content:  []message.ContentBlock{message.TextBlock{Type: "text", Text: "part3"}},
		IsFinal:  true,
		State:    message.ToolResultSuccess,
		Metadata: map[string]any{"total": 3},
	}
	close(ch)

	resp := CollectStream(ch)
	if len(resp.Content) != 3 {
		t.Fatalf("expected 3 content blocks, got %d", len(resp.Content))
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %s, want success", resp.State)
	}
	if resp.Metadata["total"] != 3 {
		t.Fatalf("metadata total = %v, want 3", resp.Metadata["total"])
	}
}

func TestIsStreamingTool(t *testing.T) {
	ft := NewFunctionTool("test", "test", nil, func(ctx context.Context, input map[string]any) (any, error) {
		return "ok", nil
	})

	if IsStreamingTool(ft) {
		t.Fatal("FunctionTool should not be a StreamingTool")
	}

	st := WrapNonStreamingTool(ft)
	if !IsStreamingTool(st) {
		t.Fatal("wrapped tool should be a StreamingTool")
	}
}

// TestCollectStream_ErrorStatePreserved locks in upstream fix #2178: once a
// final chunk reports ERROR, a trailing INTERRUPTED or DENIED final chunk must
// not overwrite it.
func TestCollectStream_ErrorStatePreserved(t *testing.T) {
	cases := []struct {
		name string
		late message.ToolResultState
	}{
		{"interrupted-after-error", message.ToolResultInterrupted},
		{"denied-after-error", message.ToolResultDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan ToolChunk, 2)
			ch <- ToolChunk{
				Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "boom"}},
				IsFinal: true,
				State:   message.ToolResultError,
			}
			ch <- ToolChunk{IsFinal: true, State: tc.late}
			close(ch)

			resp := CollectStream(ch)
			if resp.State != message.ToolResultError {
				t.Errorf("state = %q, want %q (ERROR must not be overwritten by %q)",
					resp.State, message.ToolResultError, tc.late)
			}
		})
	}
}

// A later SUCCESS final chunk still wins over an earlier ERROR only if the
// stream genuinely recovered; INTERRUPTED/DENIED are the guarded states.
func TestCollectStream_LateStateStillApplies(t *testing.T) {
	ch := make(chan ToolChunk, 2)
	ch <- ToolChunk{IsFinal: true, State: message.ToolResultInterrupted}
	ch <- ToolChunk{IsFinal: true, State: message.ToolResultSuccess}
	close(ch)
	resp := CollectStream(ch)
	if resp.State != message.ToolResultSuccess {
		t.Errorf("state = %q, want success", resp.State)
	}
}
