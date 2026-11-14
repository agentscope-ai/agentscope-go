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
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

func TestResponsesAgentPreservesCallIDs(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var requests, executions atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Input  []map[string]any `json:"input"`
					Stream bool             `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					http.Error(w, "decode", 400)
					return
				}
				step := requests.Add(1)
				if req.Stream != stream {
					t.Errorf("stream=%t", req.Stream)
				}
				if step == 2 {
					calls, results := 0, 0
					for _, item := range req.Input {
						switch item["type"] {
						case "function_call":
							calls++
							if item["id"] != "fc_1" || item["call_id"] != "call_1" {
								t.Errorf("call=%v", item)
							}
						case "function_call_output":
							results++
							if item["call_id"] != "call_1" || item["output"] != "ok" {
								t.Errorf("result=%v", item)
							}
						}
					}
					if calls != 1 || results != 1 {
						t.Errorf("calls=%d results=%d", calls, results)
					}
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					if step == 1 {
						fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"lookup\"}}\n\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{}\"}\n\n")
					} else {
						fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\n")
					}
					fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n\n")
				} else if step == 1 {
					fmt.Fprint(w, `{"id":"resp_1","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}]}`)
				} else {
					fmt.Fprint(w, `{"id":"resp_2","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
				}
			}))
			defer srv.Close()
			cm, err := model.NewOpenAIResponseModel(&model.OpenAIResponseConfig{APIKey: "fixture", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			lookup := tool.NewFunctionTool("lookup", "lookup", json.RawMessage(`{"type":"object","properties":{}}`), func(context.Context, map[string]any) (any, error) { executions.Add(1); return "ok", nil })
			a := NewUnifiedAgent("bot", "lookup then answer", cm, WithToolkit(tool.NewToolkit(lookup)), WithModelStreaming(stream))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ch, err := a.ReplyStream(ctx, "lookup")
			if err != nil {
				t.Fatal(err)
			}
			completed := false
			for e := range ch {
				if end, ok := e.(event.ReplyEndEvent); ok {
					completed = end.FinishedReason == types.ReplyCompleted
					if !completed {
						t.Errorf("reply end=%+v", end)
					}
				}
			}
			if !completed || executions.Load() != 1 || requests.Load() != 2 {
				t.Fatalf("completed=%t executions=%d requests=%d", completed, executions.Load(), requests.Load())
			}
			for _, msg := range a.state.Context {
				for _, b := range msg.GetContentBlocks() {
					switch v := b.(type) {
					case message.ToolCallBlock:
						if v.ID != "fc_1" || v.Extra["call_id"] != "call_1" {
							t.Errorf("stored call=%+v", v)
						}
					case message.ToolResultBlock:
						if v.ID != "fc_1" {
							t.Errorf("stored result=%+v", v)
						}
					}
				}
			}
		})
	}
}
