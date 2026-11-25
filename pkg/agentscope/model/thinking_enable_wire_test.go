package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestDeepSeekMoonshotThinkingWire(t *testing.T) {
	type providerCase struct {
		name           string
		newModel       func(string) (ChatModel, error)
		alwaysThinking bool
	}
	providers := []providerCase{
		{name: "deepseek", newModel: func(url string) (ChatModel, error) {
			return NewDeepSeekChatModel(DeepSeekConfig{APIKey: "fixture", Model: "deepseek-flash", BaseURL: url})
		}},
	}
	for _, name := range []string{"kimi-k2.6", "moonshot-v1-8k", "kimi-k3", "kimi-k3.1-pro", "kimi-k3.1-pro-highspeed"} {
		providers = append(providers, providerCase{
			name: name,
			newModel: func(url string) (ChatModel, error) {
				return NewMoonshotChatModel(MoonshotConfig{APIKey: "fixture", Model: name, BaseURL: url})
			},
			alwaysThinking: name == "kimi-k3" || name == "kimi-k3.1-pro" || name == "kimi-k3.1-pro-highspeed",
		})
	}
	for _, provider := range providers {
		for _, tc := range []struct {
			name string
			opts []CallOption
			want string
		}{
			{name: "unset"},
			{name: "enabled", opts: []CallOption{WithThinking(true, 0)}, want: "enabled"},
			{name: "enabled with budget", opts: []CallOption{WithThinking(true, 2048)}, want: "enabled"},
			{name: "disabled", opts: []CallOption{WithThinking(false, 0)}, want: "disabled"},
			{name: "disable helper", opts: []CallOption{WithThinkingDisabled()}, want: "disabled"},
		} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", provider.name, tc.name, stream), func(t *testing.T) {
					requests := make(chan map[string]json.RawMessage, 1)
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var body map[string]json.RawMessage
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
							http.Error(w, "decode", http.StatusBadRequest)
							return
						}
						requests <- body
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
						} else {
							fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
						}
					}))
					defer srv.Close()
					m, err := provider.newModel(srv.URL)
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), defaultModelTimeout)
					defer cancel()
					msgs := []*message.Msg{message.UserMsg("u", "hello")}
					if stream {
						ch, err := m.ChatStream(ctx, msgs, tc.opts...)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range ch {
							if chunk.Error != nil {
								t.Fatal(chunk.Error)
							}
						}
					} else if _, err := m.Chat(ctx, msgs, tc.opts...); err != nil {
						t.Fatal(err)
					}
					var body map[string]json.RawMessage
					select {
					case body = <-requests:
					default:
						t.Fatal("no HTTP request captured")
					}
					want := tc.want
					if provider.alwaysThinking {
						want = ""
					}
					raw, exists := body["thinking"]
					if want == "" {
						if exists {
							t.Fatalf("thinking must be omitted, got %s", raw)
						}
					} else {
						var thinking map[string]any
						if err := json.Unmarshal(raw, &thinking); err != nil {
							t.Fatalf("thinking = %s, want type %s: %v", raw, want, err)
						}
						if len(thinking) != 1 || thinking["type"] != want {
							t.Errorf("thinking = %s, want only type %s", raw, want)
						}
					}
					for _, field := range []string{"enable_thinking", "thinking_budget", "budget_tokens"} {
						if value, ok := body[field]; ok {
							t.Errorf("unexpected field %s = %s", field, value)
						}
					}
				})
			}
		}
	}
}
