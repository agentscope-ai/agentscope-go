package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Exercise the real adapter encoders, HTTP helpers and stream decoders. These
// are local protocol fixtures, not evidence of deployed model capabilities.
func TestManagedBuiltInAdapters(t *testing.T) {
	for _, provider := range []string{"openai", "deepseek", "xai", "moonshot", "ollama", "dashscope", "anthropic", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				stream, _ := request["stream"].(bool)
				if provider == "gemini" {
					stream = r.URL.Query().Get("alt") == "sse"
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				switch provider {
				case "anthropic":
					if stream {
						fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					} else {
						fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
					}
				case "gemini":
					data := `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`
					if stream {
						fmt.Fprintf(w, "data: %s\n\n", data)
					} else {
						fmt.Fprint(w, data)
					}
				default:
					usage := `"usage":{"prompt_tokens":10,"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":7}}`
					if stream {
						fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}],%s}\n\ndata: [DONE]\n\n", usage)
					} else {
						fmt.Fprintf(w, `{"choices":[{"message":{"content":"ok"}}],%s}`, usage)
					}
				}
			}))
			defer server.Close()
			var base ChatModel
			var err error
			api := "openai-chat"
			switch provider {
			case "openai":
				base, err = NewOpenAIChatModel(OpenAIConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
			case "deepseek":
				base, err = NewDeepSeekChatModel(DeepSeekConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
			case "xai":
				base, err = NewXAIChatModel(XAIConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
				api = "xai-chat"
			case "moonshot":
				base, err = NewMoonshotChatModel(MoonshotConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
			case "ollama":
				base, err = NewOllamaChatModel(OllamaConfig{Model: "fixture", BaseURL: server.URL})
			case "dashscope":
				base, err = NewDashScopeChatModel(DashScopeConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
			case "anthropic":
				base, err = NewAnthropicChatModel(&AnthropicConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
				api = "anthropic"
			case "gemini":
				base, err = NewGeminiChatModel(GeminiConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
				api = "gemini-chat"
			}
			if err != nil {
				t.Fatal(err)
			}
			c := inference.NewController(nil)
			defer c.Close()
			if err := c.RegisterPool(inference.PoolConfig{ID: "p", MaxActive: 1}); err != nil {
				t.Fatal(err)
			}
			d, err := c.Register(&inference.DeploymentConfig{Descriptor: inference.Descriptor{ID: "d", Provider: provider, API: api, Model: "fixture", Endpoint: server.URL}, PoolID: "p", MaxAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			m, err := NewManagedChatModel(base, d)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			_, wantThinking := base.(ThinkingDisabler)
			thinking, gotThinking := m.(ThinkingDisabler)
			if gotThinking != wantThinking {
				t.Fatal("thinking interface changed")
			}
			if gotThinking && len(thinking.DisableThinkingOptions()) == 0 {
				t.Fatal("thinking options lost")
			}
			_, wantClassifier := base.(StructuredFallbackClassifier)
			classifier, gotClassifier := m.(StructuredFallbackClassifier)
			if gotClassifier != wantClassifier {
				t.Fatal("classifier interface changed")
			}
			if gotClassifier && classifier.IsStructuredOutputFallbackError(context.Canceled) {
				t.Fatal("cancellation classified as shape rejection")
			}
			msgs := []*message.Msg{message.UserMsg("user", "hello")}
			if m.CountTokens(msgs, nil) != base.CountTokens(msgs, nil) {
				t.Fatal("token estimator changed")
			}
			if _, err := m.Chat(managedContext(), msgs); err != nil {
				t.Fatal(err)
			}
			stream, err := m.ChatStream(managedContext(), msgs)
			if err != nil {
				t.Fatal(err)
			}
			terminal := false
			for part := range stream {
				if part.Error != nil {
					t.Error(part.Error)
				}
				terminal = terminal || part.IsLast
			}
			if !terminal || d.Stats().Active != 0 {
				t.Fatal("stream did not finish cleanly")
			}
			s := d.Ledger().Snapshot()
			if len(s.Attempts) != 2 {
				t.Fatalf("attempts = %d", len(s.Attempts))
			}
			for _, a := range s.Attempts {
				output := int64(5)
				if provider == "xai" {
					output = 12
				}
				if !a.UsageComplete || a.Usage.InputTokens != 10 || a.Usage.OutputTokens != output || a.Outcome != "success" {
					t.Fatalf("raw usage or terminal lost: %+v", a)
				}
			}
		})
	}
}
