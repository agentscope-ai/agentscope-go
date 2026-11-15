package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Anthropic's input_tokens excludes cache-read/cache-creation tokens; the
// adapter must surface the inclusive total so context-window accounting sees
// the real fill level under prompt caching.
func TestAnthropicUsageTotalIncludesCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id":"msg_1","type":"message","role":"assistant",
			"content":[{"type":"text","text":"ok"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":50,"output_tokens":5,
				"cache_creation_input_tokens":300,"cache_read_input_tokens":700}
		}`)
	}))
	defer srv.Close()

	m, err := NewAnthropicChatModel(&AnthropicConfig{Model: "claude-test", APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := m.Chat(ctx, []*message.Msg{message.UserMsg("user", "hi")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage == nil {
		t.Fatal("no usage")
	}
	if resp.Usage.InputTokens != 50 {
		t.Fatalf("InputTokens = %d, want 50 (cache-exclusive, as reported)", resp.Usage.InputTokens)
	}
	if resp.Usage.TotalInputTokens != 1050 {
		t.Fatalf("TotalInputTokens = %d, want 1050", resp.Usage.TotalInputTokens)
	}
	if got := resp.Usage.EffectiveInputTokens(); got != 1050 {
		t.Fatalf("EffectiveInputTokens = %d, want 1050", got)
	}
}

// Same coverage for the streaming path: the accumulated final response must
// carry the cache-inclusive total.
func TestAnthropicStreamUsageTotalIncludesCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":50,\"output_tokens\":1,\"cache_creation_input_tokens\":300,\"cache_read_input_tokens\":700}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	m, err := NewAnthropicChatModel(&AnthropicConfig{Model: "claude-test", APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := m.ChatStream(ctx, []*message.Msg{message.UserMsg("user", "hi")})
	if err != nil {
		t.Fatal(err)
	}
	var last *ChatResponse
	for resp := range ch {
		if resp.Error != nil {
			t.Fatalf("stream error: %v", resp.Error)
		}
		if resp.IsLast {
			r := resp
			last = &r
		}
	}
	if last == nil || last.Usage == nil {
		t.Fatal("no final usage")
	}
	if last.Usage.InputTokens != 50 {
		t.Fatalf("InputTokens = %d, want 50", last.Usage.InputTokens)
	}
	if last.Usage.TotalInputTokens != 1050 {
		t.Fatalf("TotalInputTokens = %d, want 1050", last.Usage.TotalInputTokens)
	}
}
