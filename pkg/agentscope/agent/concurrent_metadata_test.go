package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// metaTool is a concurrency-safe tool that returns structured metadata in its
// response (like Edit's "diff" or a custom tool's audit payload).
type metaTool struct {
	tool.BaseTool
}

func (t *metaTool) Execute(_ context.Context, _ map[string]any) (*tool.ToolResponse, error) {
	return &tool.ToolResponse{
		Content:  []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}},
		State:    message.ToolResultSuccess,
		Metadata: map[string]any{"diff": "--- a/file\n+++ b/file\n"},
	}, nil
}

func newMetaTool() *metaTool {
	mt := &metaTool{}
	mt.ToolName = "meta_tool"
	mt.ToolDescription = "returns metadata"
	mt.ToolSchema = json.RawMessage(`{"type":"object"}`)
	mt.ConcurrencySafe = true
	return mt
}

// collectToolResultMetadata drains a reply stream and returns the metadata
// carried by each tool_result_end event, keyed by tool call ID.
func collectToolResultMetadata(t *testing.T, ch <-chan event.Event) map[string]map[string]any {
	t.Helper()
	got := map[string]map[string]any{}
	for evt := range ch {
		if e, ok := evt.(event.ToolResultEndEvent); ok {
			if e.Metadata != nil {
				got[e.ToolCallID] = e.Metadata
			}
		}
	}
	return got
}

func assertContextMetadata(t *testing.T, a *UnifiedAgent, wantIDs ...string) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	seen := map[string]bool{}
	for _, m := range a.state.Context {
		for _, b := range m.GetContentBlocks(message.ContentBlockToolResult) {
			tr, ok := b.(message.ToolResultBlock)
			if !ok {
				continue
			}
			seen[tr.ID] = true
			if tr.Metadata == nil {
				t.Errorf("tool result %s lost its metadata", tr.ID)
				continue
			}
			if diff, _ := tr.Metadata["diff"].(string); diff == "" {
				t.Errorf("tool result %s metadata missing/empty: %v", tr.ID, tr.Metadata)
			}
		}
	}
	for _, id := range wantIDs {
		if !seen[id] {
			t.Errorf("tool result %s not found in context", id)
		}
	}
}

// TestSequentialToolPreservesMetadata verifies the single-call path keeps tool
// response metadata both in the persisted context and on the event stream.
func TestSequentialToolPreservesMetadata(t *testing.T) {
	mock := &mockChatModel{
		responses: []model.ChatResponse{
			{Content: []message.ContentBlock{
				message.ToolCallBlock{Type: "tool_call", ID: "c1", Name: "meta_tool", Input: `{}`, State: message.ToolCallPending},
			}, IsLast: true},
			{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true},
		},
	}
	a := NewUnifiedAgent("test", "test", mock, WithToolkit(tool.NewToolkit(newMetaTool())))

	ch, err := a.ReplyStream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	eventMeta := collectToolResultMetadata(t, ch)
	if diff, _ := eventMeta["c1"]["diff"].(string); diff == "" {
		t.Errorf("sequential path: tool_result_end event lost metadata for c1: %v", eventMeta["c1"])
	}
	assertContextMetadata(t, a, "c1")
}

// TestConcurrentBatchPreservesToolMetadata reproduces the bug where tool
// responses carrying Metadata lost that metadata when the tool ran in a
// concurrent batch. The fix threads metadata through toolResult so both the
// persisted tool result block and the tool_result_end event keep it.
func TestConcurrentBatchPreservesToolMetadata(t *testing.T) {
	mock := &mockChatModel{
		responses: []model.ChatResponse{
			{Content: []message.ContentBlock{
				message.ToolCallBlock{Type: "tool_call", ID: "c1", Name: "meta_tool", Input: `{}`, State: message.ToolCallPending},
				message.ToolCallBlock{Type: "tool_call", ID: "c2", Name: "meta_tool", Input: `{}`, State: message.ToolCallPending},
			}, IsLast: true},
			{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true},
		},
	}
	a := NewUnifiedAgent("test", "test", mock, WithToolkit(tool.NewToolkit(newMetaTool())))

	ch, err := a.ReplyStream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	eventMeta := collectToolResultMetadata(t, ch)
	for _, id := range []string{"c1", "c2"} {
		if diff, _ := eventMeta[id]["diff"].(string); diff == "" {
			t.Errorf("concurrent path: tool_result_end event lost metadata for %s: %v", id, eventMeta[id])
		}
	}
	assertContextMetadata(t, a, "c1", "c2")
}

// streamingMetaTool emits its metadata on an intermediate chunk and none on
// the final chunk. Execute itself returns no metadata, so a test that misses
// the streaming path fails.
type streamingMetaTool struct {
	tool.BaseTool
}

func (t *streamingMetaTool) Execute(context.Context, map[string]any) (*tool.ToolResponse, error) {
	return &tool.ToolResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}},
		State:   message.ToolResultSuccess,
	}, nil
}

func (t *streamingMetaTool) ExecuteStream(context.Context, map[string]any) (<-chan tool.ToolChunk, error) {
	ch := make(chan tool.ToolChunk, 3)
	go func() {
		defer close(ch)
		ch <- tool.ToolChunk{
			Content:  []message.ContentBlock{message.TextBlock{Type: "text", Text: "partial"}},
			Metadata: map[string]any{"diff": "stale"},
		}
		ch <- tool.ToolChunk{
			Content:  []message.ContentBlock{message.TextBlock{Type: "text", Text: "more"}},
			Metadata: map[string]any{"diff": "--- a/file\n+++ b/file\n"},
		}
		ch <- tool.ToolChunk{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}},
			IsFinal: true,
			State:   message.ToolResultSuccess,
		}
	}()
	return ch, nil
}

func newStreamingMetaTool() *streamingMetaTool {
	mt := &streamingMetaTool{}
	mt.ToolName = "streaming_meta_tool"
	mt.ToolDescription = "streams metadata on a non-final chunk"
	mt.ToolSchema = json.RawMessage(`{"type":"object"}`)
	return mt
}

// TestStreamingToolPreservesLastNonNilMetadata pins the stream-folding
// convention: metadata on a non-final chunk is kept, and a later non-nil
// chunk replaces an earlier one. The final chunk carries no metadata.
func TestStreamingToolPreservesLastNonNilMetadata(t *testing.T) {
	const wantDiff = "--- a/file\n+++ b/file\n"
	mock := &mockChatModel{
		responses: []model.ChatResponse{
			{Content: []message.ContentBlock{
				message.ToolCallBlock{Type: "tool_call", ID: "c1", Name: "streaming_meta_tool", Input: `{}`, State: message.ToolCallPending},
			}, IsLast: true},
			{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true},
		},
	}
	a := NewUnifiedAgent("test", "test", mock, WithToolkit(tool.NewToolkit(newStreamingMetaTool())))

	ch, err := a.ReplyStream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	eventMeta := collectToolResultMetadata(t, ch)
	if diff, _ := eventMeta["c1"]["diff"].(string); diff != wantDiff {
		t.Errorf("streaming path: tool_result_end kept %q, want last non-nil chunk", diff)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for _, m := range a.state.Context {
		for _, b := range m.GetContentBlocks(message.ContentBlockToolResult) {
			tr, ok := b.(message.ToolResultBlock)
			if !ok || tr.ID != "c1" {
				continue
			}
			diff, _ := tr.Metadata["diff"].(string)
			if diff != wantDiff {
				t.Errorf("streaming path: persisted metadata %q, want last non-nil chunk", diff)
			}
			return
		}
	}
	t.Error("tool result c1 not found in context")
}
