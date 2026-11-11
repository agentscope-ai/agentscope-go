package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestAnthropicMergedToolHistoryWire(t *testing.T) {
	const expected = `[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"lookup","input":{}},{"type":"tool_use","id":"b","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"one"},{"type":"tool_result","tool_use_id":"b","content":"two"}]},{"role":"assistant","content":[{"type":"text","text":"done"}]}]`
	var want any
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages any  `json:"messages"`
			Stream   bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "decode", 400)
			return
		}
		if !reflect.DeepEqual(req.Messages, want) {
			t.Errorf("history=%v", req.Messages)
			http.Error(w, "invalid history", 400)
			return
		}
		if req.Stream {
			fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		} else {
			fmt.Fprint(w, `{"id":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
		}
	}))
	defer srv.Close()
	m, err := NewAnthropicChatModel(&AnthropicConfig{APIKey: "test", Model: "test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []*message.Msg{message.AssistantMsg("a", []message.ContentBlock{
		message.ToolCallBlock{Type: "tool_call", ID: "a", Name: "lookup", Input: "{}"},
		message.ToolCallBlock{Type: "tool_call", ID: "b", Name: "lookup", Input: "{}"},
		message.ToolResultBlock{Type: "tool_result", ID: "a", Output: "one"},
		message.ToolResultBlock{Type: "tool_result", ID: "b", Output: "two"},
		message.TextBlock{Type: "text", Text: "done"},
	})}
	if _, err = m.Chat(context.Background(), msgs); err != nil {
		t.Fatal(err)
	}
	ch, err := m.ChatStream(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	final := false
	for r := range ch {
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		final = final || r.IsLast
	}
	if !final {
		t.Fatal("no final response")
	}
}
