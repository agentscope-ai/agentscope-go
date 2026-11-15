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

func TestEmptyContentOnProviderWire(t *testing.T) {
	for _, provider := range []string{"anthropic", "gemini"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", provider, streaming), func(t *testing.T) {
				captured := make(chan map[string]any, 1)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode request: %v", err)
						http.Error(w, "invalid JSON", http.StatusBadRequest)
						return
					}
					captured <- body
					if provider == "gemini" {
						response := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`
						if streaming {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprintf(w, "data: %s\n\n", response)
						} else {
							fmt.Fprint(w, response)
						}
						return
					}
					if !streaming {
						fmt.Fprint(w, `{"id":"fixture","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"model\":\"fixture\",\"usage\":{\"input_tokens\":1}}}\n\n"+
						"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
						"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"+
						"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
						"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
						"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				}))
				defer srv.Close()
				var cm ChatModel
				var err error
				if provider == "anthropic" {
					cm, err = NewAnthropicChatModel(&AnthropicConfig{APIKey: "fixture", Model: "fixture", BaseURL: srv.URL})
				} else {
					cm, err = NewGeminiChatModel(GeminiConfig{APIKey: "fixture", Model: "fixture", BaseURL: srv.URL})
				}
				if err != nil {
					t.Fatal(err)
				}
				msgs := []*message.Msg{
					{Name: "system", Role: message.RoleSystem, Content: []message.ContentBlock{message.TextBlock{Type: "text"}, message.TextBlock{Type: "text"}}},
					{Name: "user", Role: message.RoleUser},
					{Name: "user", Role: message.RoleUser, Content: []message.ContentBlock{message.TextBlock{Type: "text"}}},
					{Name: "user", Role: message.RoleUser, Content: []message.ContentBlock{message.TextBlock{Type: "text"}, message.TextBlock{Type: "text"}}},
					{Name: "user", Role: message.RoleUser, Content: []message.ContentBlock{message.HintBlock{Type: "hint", Hint: ""}}},
					message.UserMsg("user", "hello"),
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if streaming {
					stream, err := cm.ChatStream(ctx, msgs)
					if err != nil {
						t.Fatal(err)
					}
					var last bool
					for response := range stream {
						if response.Error != nil {
							t.Fatal(response.Error)
						}
						last = last || response.IsLast
					}
					if !last {
						t.Fatal("fixture stream did not complete")
					}
				} else if _, err := cm.Chat(ctx, msgs); err != nil {
					t.Fatal(err)
				}
				body := <-captured
				key := "messages"
				if provider == "gemini" {
					key = "contents"
					if _, ok := body["system_instruction"]; ok {
						t.Errorf("empty system instruction sent: %#v", body["system_instruction"])
					}
				}
				contents, ok := body[key].([]any)
				if !ok || len(contents) != 1 {
					t.Fatalf("request contains empty messages: %#v", body[key])
				}
				partKey := "content"
				if provider == "gemini" {
					partKey = "parts"
				}
				parts := contents[0].(map[string]any)[partKey].([]any)
				if len(parts) != 1 || parts[0].(map[string]any)["text"] != "hello" {
					t.Fatalf("unexpected request content: %#v", parts)
				}
			})
		}
	}
}
