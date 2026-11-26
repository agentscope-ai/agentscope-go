package model

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestContentPolicyErrorContract(t *testing.T) {
	cause := errors.New("500 tokens blocked")
	e := &ContentPolicyError{AgentError: &agenterrors.AgentError{Category: agenterrors.CategoryModel, Code: "model.content_policy", Message: cause.Error(), Cause: cause}, Phase: PolicyPhaseOutput}
	if !errors.Is(e, ErrModelContentPolicy) || !errors.Is(e, cause) || IsRetryableError(fmt.Errorf("wrapped: %w", e)) || !e.RetrySamePayload() || !e.AllowFailover() {
		t.Fatal("policy contract broken")
	}
	e.PartialOutput = true
	if e.RetrySamePayload() || e.AllowFailover() {
		t.Fatal("partial output must prevent retry and failover")
	}
}

func TestContentPolicyBufferedProviders(t *testing.T) {
	cases := []struct {
		name, body, code string
		makeModel        func(string, bool) ChatModel
	}{
		{"openai", `{"id":"req","choices":[{"message":{"role":"assistant","content":null},"finish_reason":"content_filter"}]}`, "content_filter", func(url string, enabled bool) ChatModel {
			m, _ := NewOpenAIChatModel(OpenAIConfig{APIKey: "key", Model: "test", BaseURL: url, RaiseOnContentPolicy: enabled})
			return m
		}},
		{"anthropic", `{"id":"req","content":[{"type":"text","text":"I cannot help"}],"stop_reason":"refusal"}`, "refusal", func(url string, enabled bool) ChatModel {
			m, _ := NewAnthropicChatModel(&AnthropicConfig{APIKey: "key", Model: "test", BaseURL: url, RaiseOnContentPolicy: enabled})
			return m
		}},
		{"gemini", `{"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}]}`, "SAFETY", func(url string, enabled bool) ChatModel {
			m, _ := NewGeminiChatModel(GeminiConfig{APIKey: "key", Model: "test", BaseURL: url, RaiseOnContentPolicy: enabled})
			return m
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			for _, enabled := range []bool{false, true} {
				resp, err := tc.makeModel(srv.URL, enabled).Chat(context.Background(), []*message.Msg{message.NewMsg("user", message.RoleUser, "hello")})
				if err != nil {
					t.Fatal(err)
				}
				var p *ContentPolicyError
				got := errors.As(resp.Error, &p)
				if got != enabled {
					t.Fatalf("enabled=%v, response=%+v", enabled, resp)
				}
				if got && (p.OriginalCode != tc.code || p.Phase != PolicyPhaseOutput || p.PartialOutput || p.RequiresReview) {
					t.Fatalf("bad policy: %+v", p)
				}
			}
		})
	}
}

func TestContentPolicyStreamingProviders(t *testing.T) {
	cases := []struct {
		name, body, code string
		makeModel        func(string) ChatModel
	}{
		{"openai", "data: {\"id\":\"req\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n\ndata: [DONE]\n\n", "content_filter", func(url string) ChatModel {
			m, _ := NewOpenAIChatModel(OpenAIConfig{APIKey: "key", Model: "test", BaseURL: url, RaiseOnContentPolicy: true})
			return m
		}},
		{"anthropic", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"req\"}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "refusal", func(url string) ChatModel {
			m, _ := NewAnthropicChatModel(&AnthropicConfig{APIKey: "key", Model: "test", BaseURL: url, RaiseOnContentPolicy: true})
			return m
		}},
		{"gemini", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"SAFETY\"}]}\n\n", "SAFETY", func(url string) ChatModel {
			m, _ := NewGeminiChatModel(GeminiConfig{APIKey: "key", Model: "test", BaseURL: url, RaiseOnContentPolicy: true})
			return m
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			stream, err := tc.makeModel(srv.URL).ChatStream(context.Background(), []*message.Msg{message.NewMsg("user", message.RoleUser, "hello")})
			if err != nil {
				t.Fatal(err)
			}
			var last ChatResponse
			for r := range stream {
				last = r
			}
			var p *ContentPolicyError
			if !last.IsLast || !errors.As(last.Error, &p) || p.OriginalCode != tc.code || !p.PartialOutput {
				t.Fatalf("last=%+v policy=%+v", last, p)
			}
		})
	}
}

func TestContentPolicyBoundaries(t *testing.T) {
	for _, phase := range []PolicyPhase{PolicyPhaseInput, PolicyPhaseUnknown} {
		e := &ContentPolicyError{AgentError: ErrModelContentPolicy, Phase: phase}
		if e.RetrySamePayload() || !e.AllowFailover() {
			t.Fatalf("phase %q", phase)
		}
		e.RequiresReview = true
		if e.AllowFailover() || e.RetrySamePayload() {
			t.Fatal("review must prevent automatic retry and failover")
		}
	}
	for _, test := range []struct {
		provider, code string
		blocked        bool
	}{{"openai", "content_filter", true}, {"openai", "CONTENT_FILTER", false}, {"anthropic", "refusal", true}, {"gemini", "SPII", true}, {"gemini", "MAX_TOKENS", false}, {"unknown", "content_filter", false}} {
		e := classifyContentPolicy(test.provider, test.code, "id", false, nil)
		if (e != nil) != test.blocked {
			t.Fatalf("%s %s: %v", test.provider, test.code, e)
		}
		if e != nil && (e.RequiresReview || e.RequestID != "id") {
			t.Fatalf("unexpected policy %+v", e)
		}
	}
}

func TestContentPolicyAnthropicEmptyRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":"req","content":[],"stop_reason":"refusal","usage":{"input_tokens":12,"output_tokens":3}}`)
	}))
	defer srv.Close()
	m, _ := NewAnthropicChatModel(&AnthropicConfig{APIKey: "key", Model: "test", BaseURL: srv.URL, RaiseOnContentPolicy: true})
	resp, err := m.Chat(context.Background(), []*message.Msg{message.NewMsg("user", message.RoleUser, "hi")})
	var p *ContentPolicyError
	if err != nil || !errors.As(resp.Error, &p) || resp.Usage == nil || resp.Usage.InputTokens != 12 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}
