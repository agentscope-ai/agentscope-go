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

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func TestUnifiedAgentMiniMaxToolHistory(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var requests, executions atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Messages []struct {
						Role    string           `json:"role"`
						Content []map[string]any `json:"content"`
					} `json:"messages"`
					Stream bool `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					http.Error(w, "decode", 400)
					return
				}
				pending := map[string]bool{}
				results := 0
				for _, msg := range req.Messages {
					for _, block := range msg.Content {
						switch block["type"] {
						case "tool_use":
							if msg.Role != "assistant" {
								http.Error(w, "tool_use outside assistant", 400)
								return
							}
							id, _ := block["id"].(string)
							pending[id] = true
						case "tool_result":
							id, _ := block["tool_use_id"].(string)
							if msg.Role != "user" || !pending[id] || block["content"] != "18 C" {
								http.Error(w, "tool_result outside user or missing preceding call", 400)
								return
							}
							delete(pending, id)
							results++
						}
					}
				}
				step := requests.Add(1)
				if step > 2 || results != int(step-1) || len(pending) != 0 {
					http.Error(w, "incorrect tool history", 400)
					return
				}
				if !req.Stream {
					if step == 1 {
						fmt.Fprint(w, `{"id":"m","content":[{"type":"thinking","thinking":"check","signature":"sig"},{"type":"tool_use","id":"a","name":"weather","input":{"city":"Beijing"}}],"stop_reason":"tool_use"}`)
					} else {
						fmt.Fprint(w, `{"id":"m","content":[{"type":"text","text":"Forecast ready."}],"stop_reason":"end_turn"}`)
					}
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n")
				if step == 1 {
					fmt.Fprint(w, "event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"check\",\"signature\":\"sig\"}}\n\nevent: content_block_stop\ndata: {\"index\":0}\n\nevent: content_block_start\ndata: {\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"a\",\"name\":\"weather\",\"input\":{\"city\":\"Beijing\"}}}\n\nevent: content_block_stop\ndata: {\"index\":1}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n")
				} else {
					fmt.Fprint(w, "event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"Forecast ready.\"}}\n\nevent: content_block_stop\ndata: {\"index\":0}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
				}
				fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer srv.Close()
			cm, err := model.NewMiniMaxChatModel(&model.MiniMaxConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			weather := tool.NewFunctionTool("weather", "Read weather", json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`), func(_ context.Context, input map[string]any) (any, error) {
				if input["city"] != "Beijing" {
					return nil, fmt.Errorf("unexpected city")
				}
				executions.Add(1)
				return "18 C", nil
			})
			a := NewUnifiedAgent("weather", "Use weather.", cm, WithToolkit(tool.NewToolkit(weather)), WithModelStreaming(stream))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			reply, err := a.Reply(ctx, "Check Beijing weather.")
			if err != nil {
				t.Fatal(err)
			}
			text := reply.GetTextContent("")
			if text == nil || *text != "Forecast ready." || requests.Load() != 2 || executions.Load() != 1 {
				t.Fatalf("text=%v requests=%d executions=%d", text, requests.Load(), executions.Load())
			}
		})
	}
}
