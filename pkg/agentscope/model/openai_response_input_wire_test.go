package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestResponsesImageAndHintInputWire(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jS1sAAAAASUVORK5CYII="
	url := message.DataBlock{Type: "data", Source: message.URLSource{MediaType: "image/png", URL: "https://example.com/image.png"}}
	inline := message.DataBlock{Type: "data", Source: message.Base64Source{MediaType: "image/png", Data: png}}
	text := func(s string) message.TextBlock { return message.TextBlock{Type: "text", Text: s} }
	part := func(kind, key, value string) any { return map[string]any{"type": kind, key: value} }
	imageURL := part("input_image", "image_url", "https://example.com/image.png")
	imageInline := part("input_image", "image_url", "data:image/png;base64,"+png)
	item := func(role string, content any) any { return map[string]any{"role": role, "content": content} }
	unsupported := message.DataBlock{Type: "data", Source: message.Base64Source{MediaType: "audio/wav", Data: "QUJD"}}
	for _, tc := range []struct {
		name   string
		role   message.Role
		blocks []message.ContentBlock
		want   []any
	}{
		{"url_only", message.RoleUser, []message.ContentBlock{url}, []any{item("user", []any{imageURL})}},
		{"base64_only", message.RoleUser, []message.ContentBlock{inline}, []any{item("user", []any{imageInline})}},
		{"mixed_order", message.RoleUser, []message.ContentBlock{text("before"), url, inline, text("after")}, []any{item("user", []any{part("input_text", "text", "before"), imageURL, imageInline, part("input_text", "text", "after")})}},
		{"string_hint", message.RoleAssistant, []message.ContentBlock{text("before"), message.HintBlock{Type: "hint", Hint: "look"}, text("after")}, []any{item("assistant", "before"), item("user", "look"), item("assistant", "after")}},
		{"media_hint", message.RoleUser, []message.ContentBlock{message.HintBlock{Type: "hint", Hint: []message.ContentBlock{text("look"), inline, url, text("end")}}}, []any{item("user", []any{part("input_text", "text", "look"), imageInline, imageURL, part("input_text", "text", "end")})}},
		{"text_only", message.RoleUser, []message.ContentBlock{text("unchanged")}, []any{item("user", "unchanged")}},
		{"unsupported_hint", message.RoleUser, []message.ContentBlock{message.HintBlock{Type: "hint", Hint: []message.ContentBlock{unsupported, message.ThinkingBlock{Type: "thinking", Thinking: "private"}}}}, []any{item("user", "[content omitted: this turn carried only block types the Responses API cannot replay]")}},
		{"unsupported_mixed", message.RoleUser, []message.ContentBlock{text("keep"), unsupported}, []any{item("user", "keep")}},
		{"tool_boundary", message.RoleAssistant, []message.ContentBlock{text("before"), message.ToolCallBlock{Type: "tool_call", ID: "call_1", Name: "read", Input: "{}"}, message.ToolResultBlock{Type: "tool_result", ID: "call_1", Output: "done"}, message.HintBlock{Type: "hint", Hint: []message.ContentBlock{url}}}, []any{item("assistant", "before"), map[string]any{"type": "function_call", "id": "call_1", "call_id": "call_1", "name": "read", "arguments": "{}"}, map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "done"}, item("user", []any{imageURL})}},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				m := newResponsesStreamModel(t, func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						Input  []any `json:"input"`
						Stream bool  `json:"stream"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						http.Error(w, "decode", 400)
						return
					}
					if r.URL.Path != "/v1/responses" || body.Stream != stream {
						t.Errorf("request path=%s stream=%t", r.URL.Path, body.Stream)
					}
					if !reflect.DeepEqual(body.Input, tc.want) {
						t.Errorf("input=%#v, want %#v", body.Input, tc.want)
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, sseCompletedLine())
					} else {
						fmt.Fprint(w, `{"id":"resp_1","status":"completed","output":[]}`)
					}
				})
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				msgs := []*message.Msg{{Role: tc.role, Content: tc.blocks}}
				if !stream {
					if _, err := m.Chat(ctx, msgs); err != nil {
						t.Fatal(err)
					}
					return
				}
				ch, err := m.ChatStream(ctx, msgs)
				if err != nil {
					t.Fatal(err)
				}
				lasts := 0
				for response := range ch {
					if response.Error != nil {
						t.Fatal(response.Error)
					}
					if response.IsLast {
						lasts++
					}
				}
				if lasts != 1 {
					t.Fatalf("terminal responses=%d", lasts)
				}
			})
		}
	}
}
