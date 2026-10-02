package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// Exercise the real agent and adapters: a model mock that ignores history
// cannot detect the missing assistant tool_calls reported in issue #7.
func TestUnifiedAgentOpenAIToolHistory(t *testing.T) {
	providers := []struct {
		name     string
		newModel func(string) (model.ChatModel, error)
	}{
		{"openai", func(url string) (model.ChatModel, error) {
			return model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "test", Model: "test", BaseURL: url})
		}},
		{"deepseek", func(url string) (model.ChatModel, error) {
			return model.NewDeepSeekChatModel(model.DeepSeekConfig{APIKey: "test", Model: "test", BaseURL: url})
		}},
	}
	for _, provider := range providers {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ReplyStream=%t", provider.name, stream), func(t *testing.T) {
				var requests, executions atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req struct {
						Stream   bool `json:"stream"`
						Messages []struct {
							Role       string `json:"role"`
							Content    string `json:"content"`
							ToolCallID string `json:"tool_call_id"`
							ToolCalls  []struct {
								ID string `json:"id"`
							} `json:"tool_calls"`
						} `json:"messages"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					pending := map[string]bool{}
					var results int
					for _, msg := range req.Messages {
						if msg.Role == "tool" {
							if !pending[msg.ToolCallID] || msg.Content != "18 C" {
								http.Error(w, "tool result must answer a preceding assistant tool_call", http.StatusBadRequest)
								return
							}
							delete(pending, msg.ToolCallID)
							results++
							continue
						}
						if len(pending) != 0 {
							http.Error(w, "missing tool result", http.StatusBadRequest)
							return
						}
						for _, call := range msg.ToolCalls {
							if msg.Role != "assistant" || call.ID == "" || pending[call.ID] {
								http.Error(w, "invalid assistant tool call", http.StatusBadRequest)
								return
							}
							pending[call.ID] = true
						}
					}
					step := requests.Add(1)
					wantResults := []int{0, 2, 3}
					if step > 3 || results != wantResults[step-1] || len(pending) != 0 {
						http.Error(w, "tool history was lost between rounds", http.StatusBadRequest)
						return
					}
					var payload string
					switch step {
					case 1:
						payload = `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Shanghai\"}"}},{"id":"b","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Hangzhou\"}"}}]}}]}`
					case 2:
						payload = `{"choices":[{"message":{"role":"assistant","content":"One more city.","tool_calls":[{"id":"c","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Suzhou\"}"}}]}}]}`
					default:
						payload = `{"choices":[{"message":{"role":"assistant","content":"All forecasts ready."},"finish_reason":"stop"}]}`
					}
					if req.Stream {
						var response map[string]any
						if err := json.Unmarshal([]byte(payload), &response); err != nil {
							t.Error(err)
							return
						}
						choice := response["choices"].([]any)[0].(map[string]any)
						delta := choice["message"].(map[string]any)
						choice["delta"] = delta
						delete(choice, "message")
						if calls, ok := delta["tool_calls"].([]any); ok {
							for i, call := range calls {
								call.(map[string]any)["index"] = i
							}
							choice["finish_reason"] = "tool_calls"
						}
						chunk, err := json.Marshal(response)
						if err != nil {
							t.Error(err)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, payload)
					}

				}))
				defer srv.Close()
				cm, err := provider.newModel(srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				weather := tool.NewFunctionTool("weather", "Get weather", json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
					func(_ context.Context, input map[string]any) (any, error) {
						if input["city"] == "" {
							return nil, fmt.Errorf("missing city")
						}
						executions.Add(1)
						return "18 C", nil
					})
				a := NewUnifiedAgent("weather", "Use weather to compare cities.", cm, WithToolkit(tool.NewToolkit(weather)), WithModelStreaming(stream))
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if stream {
					ch, err := a.ReplyStream(ctx, "Compare the weather.")
					if err != nil {
						t.Fatal(err)
					}
					completed := false
					var text string
					for e := range ch {
						switch e := e.(type) {
						case event.TextBlockDeltaEvent:
							text += e.Delta
						case event.ReplyEndEvent:
							completed = e.FinishedReason == types.ReplyCompleted
						}
					}
					if !completed || text != "One more city.All forecasts ready." {
						t.Fatalf("completed=%v text=%q", completed, text)
					}
				} else {
					reply, err := a.Reply(ctx, "Compare the weather.")
					if err != nil {
						t.Fatal(err)
					}
					if text := reply.GetTextContent(""); text == nil || *text != "One more city.All forecasts ready." {
						t.Fatalf("reply text = %v", text)
					}
				}
				if requests.Load() != 3 || executions.Load() != 3 {
					t.Fatalf("requests=%d executions=%d", requests.Load(), executions.Load())
				}
			})
		}
	}
}
