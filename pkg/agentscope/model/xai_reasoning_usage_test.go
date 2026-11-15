package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

const xaiUsageJSON = `"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":22,"completion_tokens_details":{"reasoning_tokens":7}}`

func newXAITestServer(t *testing.T, stream bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"c1","model":"grok","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],%s}`, xaiUsageJSON)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"model\":\"grok\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprintf(w, "data: {\"id\":\"c1\",\"model\":\"grok\",\"choices\":[],%s}\n\n", xaiUsageJSON)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// Upstream #2461: xAI completion_tokens EXCLUDE reasoning tokens, so the
// xAI provider must add reasoning_tokens into OutputTokens.
func TestXAIChatAddsReasoningTokens(t *testing.T) {
	srv := newXAITestServer(t, false)
	defer srv.Close()

	m, err := NewXAIChatModel(XAIConfig{APIKey: "k", BaseURL: srv.URL, Model: "grok-4"})
	if err != nil {
		t.Fatalf("new model: %v", err)
	}
	resp, err := m.Chat(context.Background(), []*message.Msg{message.UserMsg("u", "hi")})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Usage == nil {
		t.Fatal("usage missing")
	}
	if resp.Usage.OutputTokens != 12 {
		t.Errorf("OutputTokens = %d, want 12 (5 completion + 7 reasoning)", resp.Usage.OutputTokens)
	}
	if resp.Usage.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", resp.Usage.InputTokens)
	}
}

func TestXAIChatStreamAddsReasoningTokens(t *testing.T) {
	srv := newXAITestServer(t, true)
	defer srv.Close()

	m, err := NewXAIChatModel(XAIConfig{APIKey: "k", BaseURL: srv.URL, Model: "grok-4"})
	if err != nil {
		t.Fatalf("new model: %v", err)
	}
	ch, err := m.ChatStream(context.Background(), []*message.Msg{message.UserMsg("u", "hi")})
	if err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	var last ChatResponse
	for resp := range ch {
		last = resp
	}
	if !last.IsLast {
		t.Fatal("no final response")
	}
	if last.Usage == nil || last.Usage.OutputTokens != 12 {
		t.Errorf("stream OutputTokens = %+v, want 12", last.Usage)
	}
}

// Regression guard: OpenAI-family providers already include reasoning in
// completion_tokens — adding it there would double-bill (evaluator warning
// on the #2461 port).
func TestOpenAIChatDoesNotAddReasoningTokens(t *testing.T) {
	srv := newXAITestServer(t, false)
	defer srv.Close()

	m, err := NewOpenAIChatModel(OpenAIConfig{APIKey: "k", BaseURL: srv.URL, Model: "gpt-x"})
	if err != nil {
		t.Fatalf("new model: %v", err)
	}
	resp, err := m.Chat(context.Background(), []*message.Msg{message.UserMsg("u", "hi")})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Usage == nil || resp.Usage.OutputTokens != 5 {
		t.Errorf("OutputTokens = %+v, want 5 (no reasoning addition)", resp.Usage)
	}
}
