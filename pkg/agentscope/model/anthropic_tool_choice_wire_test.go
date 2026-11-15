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

func TestMessagesToolChoiceWire(t *testing.T) {
	tool := ToolSchema{Type: "function", Function: ToolFunction{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}
	cases := []struct {
		name     string
		choice   *ToolChoice
		tools    bool
		thinking bool
		want     map[string]any
	}{
		{name: "unset", tools: true},
		{name: "empty", choice: &ToolChoice{}, tools: true},
		{name: "auto", choice: &ToolChoice{Mode: "auto"}, tools: true, want: map[string]any{"type": "auto"}},
		{name: "none", choice: &ToolChoice{Mode: "none"}, tools: true, want: map[string]any{"type": "none"}},
		{name: "none_thinking", choice: &ToolChoice{Mode: "none"}, tools: true, thinking: true, want: map[string]any{"type": "none"}},
		{name: "required", choice: &ToolChoice{Mode: "required"}, tools: true, want: map[string]any{"type": "any"}},
		{name: "named", choice: &ToolChoice{Mode: "lookup"}, tools: true, want: map[string]any{"type": "tool", "name": "lookup"}},
		{name: "none_without_tools", choice: &ToolChoice{Mode: "none"}},
	}
	for _, provider := range []string{"anthropic", "minimax"} {
		for _, tc := range cases {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", provider, tc.name, stream), func(t *testing.T) {
					requests := 0
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests++
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
							http.Error(w, "decode", 400)
							return
						}
						got, present := body["tool_choice"]
						if tc.want == nil {
							if present {
								t.Errorf("tool_choice=%v, want absent", got)
							}
						} else if !reflect.DeepEqual(got, tc.want) {
							t.Errorf("tool_choice=%v, want %v", got, tc.want)
						}
						if tc.tools {
							tools, ok := body["tools"].([]any)
							if !ok || len(tools) != 1 || tools[0].(map[string]any)["name"] != "lookup" {
								t.Errorf("tool definitions lost: %v", body["tools"])
							}
						} else if _, ok := body["tools"]; ok {
							t.Error("unexpected tools")
						}
						if body["stream"] == true {
							fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
						} else {
							fmt.Fprint(w, `{"id":"m","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`)
						}
					}))
					defer srv.Close()
					var m ChatModel
					var err error
					if provider == "anthropic" {
						m, err = NewAnthropicChatModel(&AnthropicConfig{APIKey: "test", Model: "test", BaseURL: srv.URL, MaxOutputTokens: 4096})
					} else {
						m, err = NewMiniMaxChatModel(&MiniMaxConfig{APIKey: "test", Model: "test", BaseURL: srv.URL})
					}
					if err != nil {
						t.Fatal(err)
					}
					var opts []CallOption
					if tc.tools {
						opts = append(opts, WithTools([]ToolSchema{tool}))
					}
					if tc.choice != nil {
						opts = append(opts, WithToolChoice(tc.choice))
					}
					if tc.thinking {
						opts = append(opts, WithThinking(true, 0))
					}
					msgs := []*message.Msg{message.UserMsg("u", "summarize")}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if stream {
						ch, e := m.ChatStream(ctx, msgs, opts...)
						if e != nil {
							t.Fatal(e)
						}
						final := false
						for r := range ch {
							if r.Error != nil {
								t.Fatal(r.Error)
							}
							final = final || r.IsLast
						}
						if !final {
							t.Fatal("missing final response")
						}
					} else if _, err = m.Chat(ctx, msgs, opts...); err != nil {
						t.Fatal(err)
					}
					if requests != 1 {
						t.Fatalf("requests=%d", requests)
					}
				})
			}
		}
	}
}
