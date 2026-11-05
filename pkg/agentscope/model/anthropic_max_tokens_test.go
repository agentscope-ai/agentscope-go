package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestAnthropicMaxTokensWire(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured int
		opts       []CallOption
		want       int
		thinking   any
	}{
		{"default", 0, nil, 1024, nil},
		{"configured", 8192, nil, 8192, nil},
		{"smaller_override", 8192, []CallOption{WithMaxTokens(77)}, 77, nil},
		{"larger_override", 2048, []CallOption{WithMaxTokens(8192)}, 8192, nil},
		{"last_option_wins", 8192, []CallOption{WithMaxTokens(77), WithMaxTokens(55)}, 55, nil},
		{"thinking_auto_budget", 8192, []CallOption{WithMaxTokens(4096), WithThinking(true, 0)}, 4096, map[string]any{"type": "enabled", "budget_tokens": float64(2048)}},
		{"thinking_auto_budget_minimum", 8192, []CallOption{WithMaxTokens(1536), WithThinking(true, 0)}, 1536, map[string]any{"type": "enabled", "budget_tokens": float64(1024)}},
		{"thinking_smallest_fitting_cap", 8192, []CallOption{WithMaxTokens(1025), WithThinking(true, 0)}, 1025, map[string]any{"type": "enabled", "budget_tokens": float64(1024)}},
		{"thinking_explicit_budget", 8192, []CallOption{WithThinking(true, 1024), WithMaxTokens(4096)}, 4096, map[string]any{"type": "enabled", "budget_tokens": float64(1024)}},
		{"thinking_disabled", 8192, []CallOption{WithThinking(false, 5000), WithMaxTokens(77)}, 77, map[string]any{"type": "disabled"}},
		{"legacy_budget_growth", 2048, []CallOption{WithThinking(true, 3000)}, 4024, map[string]any{"type": "enabled", "budget_tokens": float64(3000)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
					captured := make(chan map[string]any, 2)
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "test-key" {
							t.Error("unexpected Anthropic dispatch")
						}
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
							http.Error(w, "decode", 400)
							return
						}
						captured <- body
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, anthropicStreamResponseSSE())
						} else {
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprint(w, anthropicResponseJSON())
						}
					}))
					defer srv.Close()
					m, err := NewAnthropicChatModel(&AnthropicConfig{APIKey: "test-key", Model: "claude-test", BaseURL: srv.URL, MaxOutputTokens: tc.configured})
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					msgs := []*message.Msg{message.UserMsg("u", "hi")}
					call := func(opts []CallOption) {
						if !stream {
							if _, err := m.Chat(ctx, msgs, opts...); err != nil {
								t.Fatal(err)
							}
							return
						}
						ch, err := m.ChatStream(ctx, msgs, opts...)
						if err != nil {
							t.Fatal(err)
						}
						finished := false
						for r := range ch {
							if r.Error != nil {
								t.Fatal(r.Error)
							}
							finished = finished || r.IsLast
						}
						if !finished {
							t.Fatalf("no final response: %v", ctx.Err())
						}
					}
					call(tc.opts)
					body := <-captured
					if body["max_tokens"] != float64(tc.want) {
						t.Errorf("max_tokens=%v, want %d", body["max_tokens"], tc.want)
					}
					if !reflect.DeepEqual(body["thinking"], tc.thinking) {
						t.Errorf("thinking=%#v, want %#v", body["thinking"], tc.thinking)
					}
					if _, ok := body["max_completion_tokens"]; ok {
						t.Error("wrong token key")
					}
					// Reusing the same model without options must retain its config.
					call(nil)
					body = <-captured
					wantDefault := tc.configured
					if wantDefault == 0 {
						wantDefault = 1024
					}
					if body["max_tokens"] != float64(wantDefault) {
						t.Errorf("per-call override mutated config: %v", body["max_tokens"])
					}
				})
			}
		})
	}
}

func TestAnthropicMaxTokensRejectsNonpositive(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "should not dispatch", 400)
	}))
	defer srv.Close()
	m, err := NewAnthropicChatModel(&AnthropicConfig{APIKey: "test-key", Model: "m", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		for _, limit := range []int{0, -1} {
			var err error
			msgs := []*message.Msg{message.UserMsg("u", "hi")}
			if stream {
				_, err = m.ChatStream(context.Background(), msgs, WithMaxTokens(limit))
			} else {
				_, err = m.Chat(context.Background(), msgs, WithMaxTokens(limit))
			}
			if err == nil {
				t.Fatal("nonpositive output limit accepted")
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid limits dispatched %d requests", requests.Load())
	}
}

func TestAnthropicMaxTokensRejectsThinkingConflict(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "should not dispatch", 400)
	}))
	defer srv.Close()
	m, err := NewAnthropicChatModel(&AnthropicConfig{APIKey: "test-key", Model: "m", BaseURL: srv.URL, MaxOutputTokens: 8192})
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct{ limit, budget int }{{4096, 4096}, {4096, 5000}, {1024, 0}} {
			msgs := []*message.Msg{message.UserMsg("u", "hi")}
			var err error
			if stream {
				_, err = m.ChatStream(context.Background(), msgs, WithThinking(true, tc.budget), WithMaxTokens(tc.limit))
			} else {
				_, err = m.Chat(context.Background(), msgs, WithThinking(true, tc.budget), WithMaxTokens(tc.limit))
			}
			if err == nil {
				t.Fatal("conflicting thinking budget accepted")
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("conflicting budgets dispatched %d requests", requests.Load())
	}
}
