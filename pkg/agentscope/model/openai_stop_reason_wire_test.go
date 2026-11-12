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

func TestOpenAICompatibleStopReasonWire(t *testing.T) {
	providers := []struct {
		name     string
		newModel func(string) (ChatModel, error)
		output   int
	}{
		{"openai", func(url string) (ChatModel, error) {
			return NewOpenAIChatModel(OpenAIConfig{APIKey: "test", Model: "test", BaseURL: url})
		}, 3},
		{"dashscope", func(url string) (ChatModel, error) {
			return NewDashScopeChatModel(DashScopeConfig{APIKey: "test", Model: "test", BaseURL: url + "/v1"})
		}, 3},
		{"deepseek", func(url string) (ChatModel, error) {
			return NewDeepSeekChatModel(DeepSeekConfig{APIKey: "test", Model: "test", BaseURL: url})
		}, 3},
		{"moonshot", func(url string) (ChatModel, error) {
			return NewMoonshotChatModel(MoonshotConfig{APIKey: "test", Model: "test", BaseURL: url})
		}, 3},
		{"ollama", func(url string) (ChatModel, error) {
			return NewOllamaChatModel(OllamaConfig{Model: "test", BaseURL: url})
		}, 3},
		{"xai", func(url string) (ChatModel, error) {
			return NewXAIChatModel(XAIConfig{APIKey: "test", Model: "test", BaseURL: url})
		}, 5},
		{"volcengine", func(url string) (ChatModel, error) {
			return NewVolcengineChatModel(&VolcengineConfig{APIKey: "test", Model: "test", BaseURL: url + "/v1"})
		}, 3},
	}
	cases := []struct {
		name    string
		raw     any
		present bool
		want    string
	}{
		{"stop", "stop", true, StopReasonStop},
		{"length", "length", true, StopReasonLength},
		{"tools", "tool_calls", true, StopReasonToolCalls},
		{"filter", "content_filter", true, StopReasonContentFilter},
		{"legacy_function", "function_call", true, StopReasonToolCalls},
		{"compatible_alias", "max_tokens", true, StopReasonLength},
		{"normalized", " STOP ", true, StopReasonStop},
		{"unknown", "provider_specific", true, "provider_specific"},
		{"empty", "", true, ""},
		{"omitted", nil, false, ""},
		{"null", nil, true, ""},
	}
	for _, provider := range providers {
		for _, tc := range cases {
			t.Run(provider.name+"/"+tc.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" {
						t.Errorf("unexpected endpoint: %s %s", r.Method, r.URL.Path)
					}
					var req struct {
						Stream bool `json:"stream"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						http.Error(w, "decode", 400)
						return
					}
					msg := map[string]any{"role": "assistant", "content": "partial"}
					if tc.want == StopReasonToolCalls {
						msg["tool_calls"] = []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}
					}
					choice := map[string]any{"index": 0}
					if tc.present {
						choice["finish_reason"] = tc.raw
					}
					if req.Stream {
						choice["delta"] = msg
						if calls, ok := msg["tool_calls"].([]any); ok {
							calls[0].(map[string]any)["index"] = 0
						}
					} else {
						choice["message"] = msg
					}
					body := map[string]any{"id": "r1", "model": "reported-model", "choices": []any{choice}, "usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 3, "prompt_tokens_details": map[string]any{"cached_tokens": 2}, "completion_tokens_details": map[string]any{"reasoning_tokens": 2}}}
					raw, err := json.Marshal(body)
					if err != nil {
						t.Error(err)
						return
					}
					if req.Stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(raw)
					}
				}))
				defer srv.Close()
				m, err := provider.newModel(srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprint(stream), func(t *testing.T) {
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						msgs := []*message.Msg{message.UserMsg("u", "hi")}
						var result *ChatResponse
						if stream {
							ch, e := m.ChatStream(ctx, msgs)
							if e != nil {
								t.Fatal(e)
							}
							for response := range ch {
								if response.Error != nil {
									t.Fatal(response.Error)
								}
								if response.IsLast {
									copy := response
									result = &copy
								}
							}
						} else {
							result, err = m.Chat(ctx, msgs)
							if err != nil {
								t.Fatal(err)
							}
						}
						if result == nil {
							t.Fatal("missing response")
						}
						if result.StopReason != tc.want {
							t.Errorf("StopReason=%q, want %q", result.StopReason, tc.want)
						}
						if result.GetTextContent() != "partial" || result.ID != "r1" || result.ModelName != "reported-model" || !result.IsLast {
							t.Errorf("response content/identity changed: %+v", result)
						}
						if result.Usage == nil || result.Usage.InputTokens != 7 || result.Usage.OutputTokens != provider.output || result.Usage.CacheInputTokens != 2 {
							t.Errorf("usage=%+v", result.Usage)
						}
						if tc.want == StopReasonToolCalls {
							if len(result.Content) != 2 {
								t.Fatalf("tool content=%v", result.Content)
							}
							call, ok := result.Content[1].(message.ToolCallBlock)
							if !ok || call.ID != "call_1" || call.Name != "lookup" || call.Input != "{}" {
								t.Fatalf("tool call=%v", result.Content[1])
							}
						}
					})
				}
			})
		}
	}
}
