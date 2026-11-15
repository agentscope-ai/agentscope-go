package model

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func newTestResponseModel() *openaiResponseModel {
	return &openaiResponseModel{cfg: OpenAIResponseConfig{Model: "gpt-test"}}
}

// Upstream #2426: a reasoning item captured with encrypted_content must be
// replayed verbatim (nulls stripped) when the turn re-enters history.
func TestFormatInputItemsReplaysReasoningItem(t *testing.T) {
	m := newTestResponseModel()
	msg := &message.Msg{
		Role: message.RoleAssistant,
		Name: "a",
		Content: []message.ContentBlock{
			message.ThinkingBlock{
				Type:     "thinking",
				Thinking: "deep thought",
				Extra: map[string]any{"responses_item": map[string]any{
					"type":              "reasoning",
					"id":                "rs_1",
					"encrypted_content": "ENC123",
					"summary":           []any{map[string]any{"type": "summary_text", "text": "deep thought"}},
					"omit":              nil,
				}},
			},
			message.TextBlock{Type: "text", Text: "answer"},
			message.ToolCallBlock{Type: "tool_call", ID: "c1", Name: "f1", Input: `{"a":1}`},
			message.ToolCallBlock{Type: "tool_call", ID: "c2", Name: "f2", Input: `{}`},
		},
	}

	items := m.formatInputItems(msg)
	if len(items) != 4 {
		t.Fatalf("expected 4 items (reasoning, message, 2 function_calls), got %d: %v", len(items), items)
	}
	if items[0]["type"] != "reasoning" {
		t.Errorf("item[0] type = %v, want reasoning", items[0]["type"])
	}
	if items[0]["encrypted_content"] != "ENC123" {
		t.Errorf("encrypted_content lost: %v", items[0])
	}
	if _, hasNull := items[0]["omit"]; hasNull {
		t.Error("null fields must be stripped from replayed items")
	}
	if items[1]["role"] != "assistant" || items[1]["content"] != "answer" {
		t.Errorf("item[1] should be the flushed text message, got %v", items[1])
	}
	if items[2]["type"] != "function_call" || items[2]["call_id"] != "c1" {
		t.Errorf("item[2] = %v, want function_call c1", items[2])
	}
	if items[3]["type"] != "function_call" || items[3]["call_id"] != "c2" {
		t.Errorf("item[3] = %v, want function_call c2 (previously only the first survived)", items[3])
	}
}

func TestFormatInputItemsMultipleToolResults(t *testing.T) {
	m := newTestResponseModel()
	msg := &message.Msg{
		Role: message.RoleUser,
		Name: "u",
		Content: []message.ContentBlock{
			message.ToolResultBlock{Type: "tool_result", ID: "c1", Name: "f1", Output: "r1", State: message.ToolResultSuccess},
			message.ToolResultBlock{Type: "tool_result", ID: "c2", Name: "f2", Output: "r2", State: message.ToolResultSuccess},
		},
	}
	items := m.formatInputItems(msg)
	if len(items) != 2 {
		t.Fatalf("expected 2 function_call_output items, got %d", len(items))
	}
	for i, want := range []string{"c1", "c2"} {
		if items[i]["type"] != "function_call_output" || items[i]["call_id"] != want {
			t.Errorf("item[%d] = %v, want function_call_output %s", i, items[i], want)
		}
	}
}

func TestStripJSONNullsRecursive(t *testing.T) {
	in := map[string]any{
		"a": nil,
		"b": "keep",
		"c": []any{map[string]any{"d": nil, "e": 1}},
	}
	out := stripJSONNullsMap(in)
	if _, ok := out["a"]; ok {
		t.Error("nil entry not stripped")
	}
	if out["b"] != "keep" {
		t.Error("non-nil entry lost")
	}
	list := out["c"].([]any)
	inner := list[0].(map[string]any)
	if _, ok := inner["d"]; ok {
		t.Error("nested nil entry not stripped")
	}
}

func TestParseResponseKeepsReasoningItem(t *testing.T) {
	m := newTestResponseModel()
	raw := map[string]any{
		"id": "r1",
		"output": []any{
			map[string]any{
				"type":              "reasoning",
				"id":                "rs_5",
				"encrypted_content": "ENC",
				"summary":           []any{map[string]any{"type": "summary_text", "text": "s1"}, map[string]any{"type": "summary_text", "text": "s2"}},
			},
			map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "hi"}}},
		},
	}
	resp, err := m.parseResponse(raw)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	tb, ok := resp.Content[0].(message.ThinkingBlock)
	if !ok {
		t.Fatalf("content[0] = %T, want ThinkingBlock", resp.Content[0])
	}
	if tb.Thinking != "s1\ns2" {
		t.Errorf("summary text = %q, want joined s1\\ns2", tb.Thinking)
	}
	item, ok := tb.Extra["responses_item"].(map[string]any)
	if !ok || item["encrypted_content"] != "ENC" {
		t.Errorf("responses_item missing encrypted_content: %v", tb.Extra)
	}
}

func TestProcessStreamCapturesReasoningItemDone(t *testing.T) {
	m := newTestResponseModel()
	body := strings.NewReader(
		`data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_9","encrypted_content":"E9","summary":[{"type":"summary_text","text":"deep"}]}}` + "\n\n" +
			`data: {"type":"response.completed","response":{"id":"r2","usage":{"input_tokens":3,"output_tokens":4}}}` + "\n\n")
	ch := make(chan ChatResponse, 8)
	m.processStream(context.Background(), io.NopCloser(body), ch)
	var last ChatResponse
	for resp := range ch {
		if resp.IsLast {
			last = resp
		}
	}
	if !last.IsLast {
		t.Fatal("no final response")
	}
	found := false
	for _, blk := range last.Content {
		if tb, ok := blk.(message.ThinkingBlock); ok {
			item, _ := tb.Extra["responses_item"].(map[string]any)
			if item != nil && item["encrypted_content"] == "E9" {
				found = true
				if tb.Thinking != "deep" {
					t.Errorf("thinking = %q, want deep", tb.Thinking)
				}
			}
		}
	}
	if !found {
		t.Errorf("final response lost the reasoning item: %s", toJSON(last.Content))
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
