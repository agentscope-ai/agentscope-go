package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/formatter"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// OpenAIConfig.Formatter is honored: the DeepSeek formatter replays ThinkingBlock as reasoning_content.
func TestOpenAIFormatterInjectionReplaysReasoning(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	m, err := NewOpenAIChatModel(OpenAIConfig{
		APIKey:    "k",
		BaseURL:   srv.URL,
		Model:     "deepseek-chat",
		Formatter: formatter.NewDeepSeekFormatter(),
	})
	if err != nil {
		t.Fatalf("NewOpenAIChatModel: %v", err)
	}

	msgs := []*message.Msg{
		message.NewMsg("u", message.RoleUser, []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "which agents are available"},
		}),
		message.NewMsg("a", message.RoleAssistant, []message.ContentBlock{
			message.ThinkingBlock{Type: "thinking", ID: "th1", Thinking: "call list_agents first"},
			message.ToolCallBlock{Type: "tool_call", ID: "call_1", Name: "list_agents", Input: "{}", State: message.ToolCallPending},
		}),
		message.NewMsg("t", message.RoleUser, []message.ContentBlock{
			message.ToolResultBlock{Type: "tool_result", ID: "call_1", Name: "list_agents", Output: "windows-analysis"},
		}),
	}
	if _, err := m.Chat(context.Background(), msgs); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	raw, ok := got["messages"].([]any)
	if !ok {
		t.Fatalf("request has no messages: %+v", got)
	}
	var found string
	for _, item := range raw {
		msg, _ := item.(map[string]any)
		if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
			found = rc
		}
	}
	if found != "call list_agents first" {
		t.Fatalf("reasoning_content should be replayed, got %q; request=%+v", found, got)
	}
}

// Without Formatter the previous behavior is kept: no reasoning_content is sent, so providers that do not understand the field are unaffected.
func TestOpenAIDefaultFormatterOmitsReasoning(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	m, err := NewOpenAIChatModel(OpenAIConfig{APIKey: "k", BaseURL: srv.URL, Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("NewOpenAIChatModel: %v", err)
	}
	msgs := []*message.Msg{
		message.NewMsg("a", message.RoleAssistant, []message.ContentBlock{
			message.ThinkingBlock{Type: "thinking", ID: "th1", Thinking: "internal reasoning"},
			message.ToolCallBlock{Type: "tool_call", ID: "call_1", Name: "f", Input: "{}", State: message.ToolCallPending},
		}),
	}
	if _, err := m.Chat(context.Background(), msgs); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	raw := got["messages"].([]any)
	for _, item := range raw {
		msg, _ := item.(map[string]any)
		if _, ok := msg["reasoning_content"]; ok {
			t.Fatalf("default formatter must not send reasoning_content: %+v", msg)
		}
	}
}

// Parsing already handled this: reasoning_content becomes a ThinkingBlock so the next turn can replay it.
func TestParseOpenAIResponseKeepsReasoningContent(t *testing.T) {
	parsed := &openAIChatResponse{}
	raw := `{"id":"r1","choices":[{"message":{"role":"assistant","content":null,` +
		`"reasoning_content":"let me think","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`
	if err := json.Unmarshal([]byte(raw), parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	resp, err := parseOpenAIResponse(parsed, nil)
	if err != nil {
		t.Fatalf("parseOpenAIResponse: %v", err)
	}
	var got string
	for _, blk := range resp.Content {
		if tb, ok := blk.(message.ThinkingBlock); ok {
			got = tb.Thinking
		}
	}
	if got != "let me think" {
		t.Fatalf("reasoning_content should become a ThinkingBlock, got %q", got)
	}
}
