package model

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// fullStream is a realistic Responses SSE transcript: a reasoning item, two
// text deltas, then two function calls in a known order.
const fullStream = `data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","encrypted_content":"E1","summary":[{"type":"summary_text","text":"deep"}]}}` + "\n\n" +
	`data: {"type":"response.output_text.delta","delta":"hello "}` + "\n\n" +
	`data: {"type":"response.output_text.delta","delta":"world"}` + "\n\n" +
	`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"c1","name":"f1"}}` + "\n\n" +
	`data: {"type":"response.function_call_arguments.delta","item_id":"c1","delta":"{\"a\":1}"}` + "\n\n" +
	`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"c2","name":"f2"}}` + "\n\n" +
	`data: {"type":"response.function_call_arguments.delta","item_id":"c2","delta":"{}"}` + "\n\n"

func drainResponsesStream(t *testing.T, body string) ChatResponse {
	t.Helper()
	m := newTestResponseModel()
	ch := make(chan ChatResponse, 64)
	m.processStream(context.Background(), io.NopCloser(strings.NewReader(body)), ch)
	var last ChatResponse
	saw := false
	for resp := range ch {
		if resp.IsLast {
			last, saw = resp, true
		}
	}
	if !saw {
		t.Fatal("no final response")
	}
	return last
}

// The streaming path is the agent's DEFAULT path (ReplyStream), so its block
// order is what lands in history and gets replayed. parseResponse (the
// non-streaming path) walks the output array in order: reasoning, text, tool
// calls. Emitting text before reasoning replays as message(text) -> reasoning,
// i.e. a reasoning item with nothing after it — exactly the shape upstream
// #2426 exists to prevent.
func TestProcessStreamOrdersReasoningBeforeText(t *testing.T) {
	last := drainResponsesStream(t, fullStream+
		`data: {"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":3,"output_tokens":4}}}`+"\n\n")

	if len(last.Content) != 4 {
		t.Fatalf("content = %d blocks, want 4: %s", len(last.Content), toJSON(last.Content))
	}
	if _, ok := last.Content[0].(message.ThinkingBlock); !ok {
		t.Errorf("content[0] = %T, want ThinkingBlock first", last.Content[0])
	}
	tb, ok := last.Content[1].(message.TextBlock)
	if !ok {
		t.Fatalf("content[1] = %T, want TextBlock after reasoning", last.Content[1])
	}
	if tb.Text != "hello world" {
		t.Errorf("text = %q, want the accumulated deltas", tb.Text)
	}
	// Tool calls must keep the order the API produced them in; ranging a map
	// would randomize it and break multi-call replay.
	for i, want := range []string{"c1", "c2"} {
		tc, ok := last.Content[2+i].(message.ToolCallBlock)
		if !ok {
			t.Fatalf("content[%d] = %T, want ToolCallBlock", 2+i, last.Content[2+i])
		}
		if tc.ID != want {
			t.Errorf("content[%d] id = %q, want %q", 2+i, tc.ID, want)
		}
	}

	// The real consequence: replaying this exact content must put the
	// reasoning item before the message that follows it.
	m := newTestResponseModel()
	msg := &message.Msg{Role: message.RoleAssistant, Name: "a", Content: last.Content}
	items := m.formatInputItems(msg)
	if len(items) != 4 {
		t.Fatalf("replay produced %d items, want 4: %v", len(items), items)
	}
	if items[0]["type"] != "reasoning" {
		t.Errorf("replayed item[0] = %v, want the reasoning item", items[0])
	}
	if items[1]["role"] != "assistant" {
		t.Errorf("replayed item[1] = %v, want the assistant message", items[1])
	}
	if items[2]["type"] != "function_call" || items[2]["call_id"] != "c1" {
		t.Errorf("replayed item[2] = %v, want function_call c1", items[2])
	}
	if items[3]["type"] != "function_call" || items[3]["call_id"] != "c2" {
		t.Errorf("replayed item[3] = %v, want function_call c2", items[3])
	}
}

// A truncated stream (no response.completed) still delivers a final response;
// it must use the same ordering so partial history replays consistently.
func TestProcessStreamTruncatedKeepsOrder(t *testing.T) {
	last := drainResponsesStream(t, fullStream)
	if last.Error == nil {
		t.Error("a stream without response.completed should surface an Error")
	}
	if len(last.Content) != 4 {
		t.Fatalf("content = %d blocks, want 4: %s", len(last.Content), toJSON(last.Content))
	}
	if _, ok := last.Content[0].(message.ThinkingBlock); !ok {
		t.Errorf("content[0] = %T, want ThinkingBlock first", last.Content[0])
	}
	if _, ok := last.Content[1].(message.TextBlock); !ok {
		t.Errorf("content[1] = %T, want TextBlock second", last.Content[1])
	}
}

// A text-only turn must not replay as message -> reasoning with nothing after
// the reasoning item.
func TestProcessStreamTextOnlyTurnReplaysCleanly(t *testing.T) {
	last := drainResponsesStream(t,
		`data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"thought"}]}}`+"\n\n"+
			`data: {"type":"response.output_text.delta","delta":"answer"}`+"\n\n"+
			`data: {"type":"response.completed","response":{"id":"r2"}}`+"\n\n")
	if len(last.Content) != 2 {
		t.Fatalf("content = %d blocks, want 2: %s", len(last.Content), toJSON(last.Content))
	}
	if _, ok := last.Content[0].(message.ThinkingBlock); !ok {
		t.Fatalf("content[0] = %T, want ThinkingBlock", last.Content[0])
	}
	m := newTestResponseModel()
	items := m.formatInputItems(&message.Msg{Role: message.RoleAssistant, Name: "a", Content: last.Content})
	if len(items) != 2 {
		t.Fatalf("replay produced %d items, want 2: %v", len(items), items)
	}
	if items[0]["type"] != "reasoning" {
		t.Errorf("replayed item[0] = %v, want reasoning first", items[0])
	}
	if items[1]["content"] != "answer" {
		t.Errorf("replayed item[1] = %v, want the text message", items[1])
	}
}

// A message whose blocks the Responses API cannot carry must stay visible in
// the request; emitting zero items silently dropped the turn.
func TestFormatInputItemsNeverDropsATurn(t *testing.T) {
	m := newTestResponseModel()
	msg := &message.Msg{
		Role: message.RoleUser,
		Name: "u",
		Content: []message.ContentBlock{
			// A thinking block with no replayable item, plus an audio block.
			message.ThinkingBlock{Type: "thinking", Thinking: "plain thought"},
			message.DataBlock{Type: "data", ID: "a1",
				Source: message.Base64Source{Type: "base64", Data: "AA==", MediaType: "audio/wav"}},
		},
	}
	items := m.formatInputItems(msg)
	if len(items) != 1 {
		t.Fatalf("items = %d, want exactly 1 placeholder item: %v", len(items), items)
	}
	content, _ := items[0]["content"].(string)
	if !strings.Contains(content, "omitted") {
		t.Errorf("placeholder content = %q, want an explicit omission note", content)
	}
}

// An empty reasoning item must not be replayed as "{}", which some
// Responses-compatible APIs reject for the whole request.
func TestFormatInputItemsSkipsEmptyReasoningItem(t *testing.T) {
	m := newTestResponseModel()
	msg := &message.Msg{
		Role: message.RoleAssistant,
		Name: "a",
		Content: []message.ContentBlock{
			message.ThinkingBlock{Type: "thinking", Thinking: "x",
				Extra: map[string]any{"responses_item": map[string]any{}}},
			message.TextBlock{Type: "text", Text: "answer"},
		},
	}
	items := m.formatInputItems(msg)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (the empty reasoning item dropped): %v", len(items), items)
	}
	if items[0]["content"] != "answer" {
		t.Errorf("item = %v, want the text message", items[0])
	}
}
