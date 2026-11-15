package formatter

import (
	"encoding/json"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestOpenAIFormatter_TextMessage(t *testing.T) {
	f := NewOpenAIFormatter()
	msgs := []*message.Msg{
		message.SystemMsg("bot", "You are helpful."),
		message.UserMsg("user", "Hello"),
	}
	result, err := f.Format(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0]["role"] != "system" {
		t.Errorf("msg[0].role = %v", result[0]["role"])
	}
	if result[0]["content"] != "You are helpful." {
		t.Errorf("msg[0].content = %v", result[0]["content"])
	}
	if result[1]["role"] != "user" {
		t.Errorf("msg[1].role = %v", result[1]["role"])
	}
}

func TestOpenAIFormatter_ToolCall(t *testing.T) {
	f := NewOpenAIFormatter()
	msg := message.AssistantMsg("bot", []message.ContentBlock{
		message.ToolCallBlock{Type: "tool_call", ID: "call_1", Name: "search", Input: `{"q":"go"}`},
	})
	result, err := f.Format([]*message.Msg{msg})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	tc, ok := result[0]["tool_calls"].([]map[string]any)
	if !ok || len(tc) != 1 {
		t.Fatalf("tool_calls = %v", result[0]["tool_calls"])
	}
	fn := tc[0]["function"].(map[string]any)
	if fn["name"] != "search" {
		t.Errorf("function.name = %v", fn["name"])
	}
}

func TestOpenAIFormatter_ToolResult(t *testing.T) {
	f := NewOpenAIFormatter()
	msg := message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
		message.ToolResultBlock{Type: "tool_result", ID: "call_1", Name: "search", Output: "found it"},
	})
	result, err := f.Format([]*message.Msg{msg})
	if err != nil {
		t.Fatal(err)
	}
	if result[0]["role"] != "tool" {
		t.Errorf("role = %v, want 'tool'", result[0]["role"])
	}
	if result[0]["tool_call_id"] != "call_1" {
		t.Errorf("tool_call_id = %v", result[0]["tool_call_id"])
	}
}

func TestDashScopeFormatter_Thinking(t *testing.T) {
	f := NewDashScopeFormatter()
	msg := message.AssistantMsg("bot", []message.ContentBlock{
		message.ThinkingBlock{Type: "thinking", Thinking: "Let me think..."},
		message.TextBlock{Type: "text", Text: "The answer is 42."},
	})
	result, err := f.Format([]*message.Msg{msg})
	if err != nil {
		t.Fatal(err)
	}
	if result[0]["reasoning_content"] != "Let me think..." {
		t.Errorf("reasoning_content = %v", result[0]["reasoning_content"])
	}
	if result[0]["content"] != "The answer is 42." {
		t.Errorf("content = %v", result[0]["content"])
	}
}

func TestAnthropicFormatter_SkipsSystem(t *testing.T) {
	f := NewAnthropicFormatter()
	msgs := []*message.Msg{
		message.SystemMsg("bot", "System prompt"),
		message.UserMsg("user", "Hello"),
	}
	result, err := f.Format(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 message (system skipped), got %d", len(result))
	}
	if result[0]["role"] != "user" {
		t.Errorf("role = %v", result[0]["role"])
	}
}

func TestAnthropicFormatter_ToolUse(t *testing.T) {
	f := NewAnthropicFormatter()
	msg := message.AssistantMsg("bot", []message.ContentBlock{
		message.TextBlock{Type: "text", Text: "Let me search."},
		message.ToolCallBlock{Type: "tool_call", ID: "tu_1", Name: "search", Input: `{"q":"test"}`},
	})
	result, err := f.Format([]*message.Msg{msg})
	if err != nil {
		t.Fatal(err)
	}
	content, ok := result[0]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content type = %T", result[0]["content"])
	}
	if len(content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(content))
	}
	if content[0]["type"] != "text" {
		t.Errorf("block[0].type = %v", content[0]["type"])
	}
	if content[1]["type"] != "tool_use" {
		t.Errorf("block[1].type = %v", content[1]["type"])
	}
	if content[1]["name"] != "search" {
		t.Errorf("block[1].name = %v", content[1]["name"])
	}
}

func TestAnthropicFormatter_ToolResult(t *testing.T) {
	f := NewAnthropicFormatter()
	msg := message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
		message.ToolResultBlock{Type: "tool_result", ID: "tu_1", Output: "result text"},
	})
	result, err := f.Format([]*message.Msg{msg})
	if err != nil {
		t.Fatal(err)
	}
	content := result[0]["content"].([]map[string]any)
	if content[0]["type"] != "tool_result" {
		t.Errorf("type = %v", content[0]["type"])
	}
	if content[0]["tool_use_id"] != "tu_1" {
		t.Errorf("tool_use_id = %v", content[0]["tool_use_id"])
	}
}

func TestExtractSystemPrompt(t *testing.T) {
	msgs := []*message.Msg{
		message.SystemMsg("bot", "Be helpful."),
		message.UserMsg("user", "Hi"),
	}
	s := ExtractSystemPrompt(msgs)
	if s != "Be helpful." {
		t.Errorf("system prompt = %q", s)
	}

	s = ExtractSystemPrompt([]*message.Msg{message.UserMsg("user", "Hi")})
	if s != "" {
		t.Errorf("expected empty, got %q", s)
	}
}

// Verify JSON serialization is valid
func TestOpenAIFormatter_ValidJSON(t *testing.T) {
	f := NewOpenAIFormatter()
	msgs := []*message.Msg{
		message.UserMsg("user", "test"),
		message.AssistantMsg("bot", []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "c1", Name: "fn", Input: `{"a":1}`},
		}),
		message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
			message.ToolResultBlock{Type: "tool_result", ID: "c1", Output: "ok"},
		}),
	}
	result, err := f.Format(msgs)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("JSON marshal failed: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("empty JSON")
	}
}
