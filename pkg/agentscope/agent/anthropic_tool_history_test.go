package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

type anthropicHistoryTransport func(*http.Request) (*http.Response, error)

func (f anthropicHistoryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUnifiedAgentAnthropicToolHistory(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			requests := 0
			executions := 0
			invalid := false
			client := &http.Client{Transport: anthropicHistoryTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				var body struct {
					Stream   bool `json:"stream"`
					Messages []struct {
						Role    string `json:"role"`
						Content []struct {
							Type      string `json:"type"`
							ID        string `json:"id"`
							ToolUseID string `json:"tool_use_id"`
							Content   string `json:"content"`
						} `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					return nil, err
				}
				results := 0
				for _, msg := range body.Messages {
					for _, block := range msg.Content {
						if block.Type == "tool_result" {
							results++
							if msg.Role != "user" || block.ToolUseID != "toolu_1" || block.Content != "ok" {
								invalid = true
							}
						}
					}
				}
				if requests == 2 && results != 1 {
					invalid = true
				}
				status := 200
				payload := `{"id":"m1","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]}`
				if body.Stream {
					payload = "event: message_start\ndata: {\"message\":{\"id\":\"m1\"}}\n\nevent: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"lookup\",\"input\":{}}}\n\nevent: content_block_stop\ndata: {\"index\":0}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				if requests > 1 {
					payload = `{"id":"m2","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}`
					if body.Stream {
						payload = "event: message_start\ndata: {\"message\":{\"id\":\"m2\"}}\n\nevent: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"done\"}}\n\nevent: content_block_stop\ndata: {\"index\":0}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					}
				}
				if invalid {
					status = 400
					payload = `{"error":{"type":"invalid_request_error","message":"tool_result must be in user message"}}`
				}
				if requests > 3 {
					return nil, fmt.Errorf("too many requests")
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(payload)), Request: r}, nil
			})}
			m, err := model.NewAnthropicChatModel(&model.AnthropicConfig{APIKey: "test", Model: "claude-sonnet-4-5", HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			lookup := tool.NewFunctionTool("lookup", "lookup", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) {
				executions++
				return "ok", nil
			})
			a := NewUnifiedAgent("a", "Use lookup", m, WithToolkit(tool.NewToolkit(lookup)), WithModelStreaming(stream))
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if stream {
				ch, e := a.ReplyStream(ctx, "lookup")
				if e != nil {
					t.Fatal(e)
				}
				completed := false
				text := ""
				for e := range ch {
					switch v := e.(type) {
					case event.TextBlockDeltaEvent:
						text += v.Delta
					case event.ReplyEndEvent:
						completed = v.FinishedReason == types.ReplyCompleted
					}
				}
				if !completed || text != "done" {
					t.Fatalf("completed=%v text=%q", completed, text)
				}
			} else {
				reply, e := a.Reply(ctx, "lookup")
				if e != nil {
					t.Fatal(e)
				}
				text := reply.GetTextContent("")
				if text == nil || *text != "done" {
					t.Fatalf("reply=%v", reply)
				}
			}
			if requests != 2 || executions != 1 {
				t.Fatalf("requests=%d executions=%d", requests, executions)
			}
			if invalid {
				t.Errorf("real agent emitted assistant tool_result on request %d", requests)
			}
		})
	}
}
