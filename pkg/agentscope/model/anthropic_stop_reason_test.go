package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Use the HTTP boundary rather than a decoded Go struct: the buffered adapter
// must both decode the wire field and carry it into the public ChatResponse.
func TestAnthropicChatStopReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire any
		want string
	}{
		{"end_turn", "end_turn", StopReasonStop},
		{"tool_use", "tool_use", StopReasonToolCalls},
		{"max_tokens", "max_tokens", StopReasonLength},
		{"stop_sequence", "stop_sequence", "stop_sequence"},
		{"pause_turn", "pause_turn", "pause_turn"},
		{"refusal", "refusal", "refusal"},
		{"future_reason", "provider_future_reason", "provider_future_reason"},
		{"empty", "", ""},
		{"null", nil, ""},
		{"missing", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Streaming is the existing control for the shared reason vocabulary.
			for _, stream := range []bool{true, false} {
				t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
					content := map[string]any{"type": "text", "text": "answer"}
					switch tc.name {
					case "max_tokens":
						content = map[string]any{"type": "thinking", "thinking": "unfinished reasoning", "signature": "sig"}
					case "tool_use":
						content = map[string]any{"type": "tool_use", "id": "call-1", "name": "weather", "input": map[string]any{"city": "Beijing"}}
					}
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "test-key" {
							t.Error("unexpected Anthropic dispatch")
						}
						usage := map[string]any{"input_tokens": 13, "output_tokens": 9, "cache_read_input_tokens": 7}
						if !stream {
							w.Header().Set("Content-Type", "application/json")
							body := map[string]any{"id": "msg-1", "model": "claude-test", "role": "assistant", "content": []any{content}, "usage": usage}
							if tc.name != "missing" {
								body["stop_reason"] = tc.wire
							}
							if err := json.NewEncoder(w).Encode(body); err != nil {
								t.Error(err)
							}
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						events := []struct {
							name string
							data any
						}{
							{"message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg-1", "model": "claude-test", "usage": usage}}},
							{"content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": content}},
							{"content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}},
						}
						delta := map[string]any{}
						if tc.name != "missing" {
							delta["stop_reason"] = tc.wire
						}
						events = append(events, struct {
							name string
							data any
						}{"message_delta", map[string]any{"type": "message_delta", "delta": delta, "usage": usage}}, struct {
							name string
							data any
						}{"message_stop", map[string]any{"type": "message_stop"}})
						for _, e := range events {
							data, err := json.Marshal(e.data)
							if err != nil {
								t.Error(err)
								return
							}
							fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, data)
						}
					}))
					defer srv.Close()
					m, err := NewAnthropicChatModel(&AnthropicConfig{APIKey: "test-key", Model: "claude-test", BaseURL: srv.URL})
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					msgs := []*message.Msg{message.UserMsg("user", "hello")}
					var response *ChatResponse
					if stream {
						ch, err := m.ChatStream(ctx, msgs)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range ch {
							if chunk.Error != nil {
								t.Fatal(chunk.Error)
							}
							if chunk.IsLast {
								cp := chunk
								response = &cp
							}
						}
					} else {
						response, err = m.Chat(ctx, msgs)
						if err != nil {
							t.Fatal(err)
						}
					}
					if response == nil {
						t.Fatalf("missing final response: %v", ctx.Err())
					}
					if response.StopReason != tc.want {
						t.Errorf("StopReason=%q, want %q", response.StopReason, tc.want)
					}
					if response.ID != "msg-1" || response.ModelName != "claude-test" || !response.IsLast || len(response.Content) != 1 {
						t.Fatalf("response metadata/content lost: %+v", response)
					}
					if response.Usage == nil || response.Usage.InputTokens != 13 || response.Usage.OutputTokens != 9 || response.Usage.CacheInputTokens != 7 {
						t.Fatalf("usage lost: %+v", response.Usage)
					}
					switch tc.name {
					case "max_tokens":
						block, ok := response.Content[0].(message.ThinkingBlock)
						if !ok || block.Thinking != "unfinished reasoning" || block.Extra["signature"] != "sig" || response.GetTextContent() != "" {
							t.Fatalf("reasoning-only content changed: %+v", response.Content)
						}
					case "tool_use":
						block, ok := response.Content[0].(message.ToolCallBlock)
						if !ok || block.ID != "call-1" || block.Name != "weather" || block.Input != `{"city":"Beijing"}` {
							t.Fatalf("tool call changed: %+v", response.Content)
						}
					default:
						if response.GetTextContent() != "answer" {
							t.Fatalf("text changed: %+v", response.Content)
						}
					}
				})
			}
		})
	}
}
