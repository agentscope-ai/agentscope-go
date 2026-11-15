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

// Opaque values are synthetic sentinels, not valid provider signatures.
func TestAnthropicThinkingRoundTrip(t *testing.T) {
	const blocks = `[{"type":"thinking","thinking":"consider","signature":"sig-start"},{"type":"redacted_thinking","data":"opaque-data"},{"type":"thinking","thinking":"","signature":"sig-empty"},{"type":"text","text":"checking"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]`
	var want []map[string]any
	if err := json.Unmarshal([]byte(blocks), &want); err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
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
				if requests == 2 {
					if len(req.Messages) != 3 || req.Messages[1].Role != "assistant" || !reflect.DeepEqual(req.Messages[1].Content, want) {
						t.Errorf("replayed history=%v, want blocks=%v", req.Messages, want)
					}
					if req.Stream {
						fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					} else {
						fmt.Fprint(w, `{"id":"m2","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`)
					}
					return
				}
				if !req.Stream {
					fmt.Fprintf(w, `{"id":"m1","content":%s,"stop_reason":"tool_use"}`, blocks)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: message_start\ndata: {\"message\":{\"id\":\"m1\"}}\n\n")
				for i, b := range want {
					start := make(map[string]any)
					for k, v := range b {
						start[k] = v
					}
					signature, _ := start["signature"].(string)
					// Exercise both an initial signature and fragmented signature deltas.
					if i == 2 {
						delete(start, "signature")
					}
					raw, _ := json.Marshal(map[string]any{"index": i, "content_block": start})
					fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", raw)
					if i == 2 {
						for _, part := range []string{signature[:4], signature[4:]} {
							raw, _ := json.Marshal(map[string]any{"index": i, "delta": map[string]any{"type": "signature_delta", "signature": part}})
							fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", raw)
						}
					}
					fmt.Fprintf(w, "event: content_block_stop\ndata: {\"index\":%d}\n\n", i)
				}
				fmt.Fprint(w, "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer srv.Close()
			m, err := NewAnthropicChatModel(&AnthropicConfig{APIKey: "test", Model: "test", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			msgs := []*message.Msg{message.UserMsg("u", "lookup")}
			var resp *ChatResponse
			if stream {
				ch, e := m.ChatStream(context.Background(), msgs)
				if e != nil {
					t.Fatal(e)
				}
				for r := range ch {
					if r.Error != nil {
						t.Fatal(r.Error)
					}
					if r.IsLast {
						v := r
						resp = &v
					}
				}
			} else {
				resp, err = m.Chat(context.Background(), msgs)
				if err != nil {
					t.Fatal(err)
				}
			}
			if resp == nil || len(resp.Content) != len(want) {
				t.Fatalf("response=%+v", resp)
			}
			redacted, ok := resp.Content[1].(message.RedactedThinkingBlock)
			if !ok || redacted.Data != "opaque-data" {
				t.Fatalf("redacted=%+v", resp.Content[1])
			}
			empty, ok := resp.Content[2].(message.ThinkingBlock)
			if !ok || empty.Thinking != "" || empty.Extra["signature"] != "sig-empty" {
				t.Fatalf("signed empty=%+v", resp.Content[2])
			}
			// Persist/reload the message before replay to exercise the existing format.
			raw, e := json.Marshal(message.AssistantMsg("a", resp.Content))
			if e != nil {
				t.Fatal(e)
			}
			var assistant message.Msg
			if e = json.Unmarshal(raw, &assistant); e != nil {
				t.Fatal(e)
			}
			msgs = append(msgs, &assistant, message.NewMsg("tool", message.Role("tool"), []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: "toolu_1", Output: "ok"}}))
			if stream {
				ch, e := m.ChatStream(context.Background(), msgs)
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
			} else if _, err = m.Chat(context.Background(), msgs); err != nil {
				t.Fatal(err)
			}
			if requests != 2 {
				t.Fatalf("requests=%d", requests)
			}
		})
	}
}
