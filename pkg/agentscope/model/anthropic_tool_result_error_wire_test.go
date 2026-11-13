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

// The outbound Messages request must carry is_error only for failed tool
// results (upstream #3905): error state and empty error output both gain the
// flag, a successful result keeps the plain tool_result shape.
func TestAnthropicToolResultErrorWire(t *testing.T) {
	cases := []struct {
		name   string
		state  message.ToolResultState
		output string
		want   string // exact JSON of the last wire message
	}{
		{
			name:   "error",
			state:  message.ToolResultError,
			output: "failed",
			want:   `{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"failed","is_error":true}]}`,
		},
		{
			name:   "error-empty-output",
			state:  message.ToolResultError,
			output: "",
			want:   `{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"(empty tool output)","is_error":true}]}`,
		},
		{
			name:   "success",
			state:  message.ToolResultSuccess,
			output: "failed",
			want:   `{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"failed"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured []any
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
				captured = append(captured, req.Messages)
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
				message.ToolCallBlock{Type: "tool_call", ID: "call_1", Name: "validate", Input: "{}"},
				message.ToolResultBlock{Type: "tool_result", ID: "call_1", Name: "validate", Output: tc.output, State: tc.state},
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
				t.Fatal("no final stream response")
			}
			if len(captured) != 2 {
				t.Fatalf("captured %d requests, want 2 (buffered + streamed)", len(captured))
			}
			var want any
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			for i, body := range captured {
				wire, ok := body.([]any)
				if !ok || len(wire) != 2 {
					t.Fatalf("request %d messages = %v", i, body)
				}
				if !reflect.DeepEqual(wire[1], want) {
					t.Fatalf("request %d last message = %v, want %s", i, wire[1], tc.want)
				}
			}
		})
	}
}
