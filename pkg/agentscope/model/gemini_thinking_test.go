package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestGeminiThinkingWire(t *testing.T) {
	tests := []struct {
		name, model string
		opts        []CallOption
		want        map[string]any
		errText     string
	}{
		{name: "unset", model: "gemini-2.5-pro"},
		{name: "budget", model: "gemini-2.5-flash", opts: []CallOption{WithThinking(true, 2048)}, want: map[string]any{"includeThoughts": true, "thinkingBudget": float64(2048)}},
		{name: "default budget", model: "gemini-2.5-flash", opts: []CallOption{WithThinking(true, 0)}, want: map[string]any{"includeThoughts": true, "thinkingBudget": float64(1024)}},
		{name: "negative option budget", model: "gemini-2.5-flash", opts: []CallOption{WithThinking(true, -1)}, want: map[string]any{"includeThoughts": true, "thinkingBudget": float64(1024)}},
		{name: "disabled", model: "gemini-2.5-flash", opts: []CallOption{WithThinkingDisabled()}, want: map[string]any{"includeThoughts": false, "thinkingBudget": float64(0)}},
		{name: "false ignores budget", model: "gemini-2.5-flash", opts: []CallOption{WithThinking(false, 2048)}, want: map[string]any{"includeThoughts": false, "thinkingBudget": float64(0)}},
		{name: "argon level", model: "gemini-4-argon", opts: []CallOption{WithThinking(true, 0), WithThinkingLevel("low")}, want: map[string]any{"includeThoughts": true, "thinkingLevel": "LOW"}},
		{name: "qualified carbon", model: "publishers/google/models/gemini-4-carbon", opts: []CallOption{WithThinking(true, 0), WithThinkingLevel("mEdIuM")}, want: map[string]any{"includeThoughts": true, "thinkingLevel": "MEDIUM"}},
		{name: "level alone", model: "gemini-4-carbon", opts: []CallOption{WithThinkingLevel("HIGH")}, want: map[string]any{"thinkingLevel": "HIGH"}},
		{name: "argon default", model: "gemini-4-argon", opts: []CallOption{WithThinking(true, 0)}, want: map[string]any{"includeThoughts": true}},
		{name: "argon unset", model: "gemini-4-argon"},
		{name: "argon disabled", model: "gemini-4-argon", opts: []CallOption{WithThinkingDisabled()}, want: map[string]any{"includeThoughts": false}},
		{name: "hydrogen budget era", model: "gemini-4-hydrogen", opts: []CallOption{WithThinking(true, 0)}, want: map[string]any{"includeThoughts": true, "thinkingBudget": float64(1024)}},
		{name: "suffix not matched", model: "gemini-4-argon-preview", opts: []CallOption{WithThinkingLevel("LOW")}, errText: "thinking level"},
		{name: "wrong case not matched", model: "Gemini-4-argon", opts: []CallOption{WithThinkingLevel("LOW")}, errText: "thinking level"},
		{name: "budget rejected", model: "gemini-4-carbon", opts: []CallOption{WithThinking(true, 1)}, errText: "thinking budget"},
		{name: "disabled budget rejected", model: "gemini-4-argon", opts: []CallOption{WithThinking(false, 1)}, errText: "thinking budget"},
		{name: "level rejected", model: "gemini-2.5-flash", opts: []CallOption{WithThinkingLevel("LOW")}, errText: "thinking level"},
		{name: "invalid level", model: "gemini-4-argon", opts: []CallOption{WithThinkingLevel("MINIMAL")}, errText: "thinking level"},
		{name: "invalid xhigh", model: "gemini-4-carbon", opts: []CallOption{WithThinkingLevel("XHIGH")}, errText: "thinking level"},
		{name: "empty level", model: "gemini-4-carbon", opts: []CallOption{WithThinkingLevel("")}, errText: "thinking level"},
		{name: "disabled explicit level", model: "gemini-4-argon", opts: []CallOption{WithThinkingDisabled(), WithThinkingLevel("LOW")}, want: map[string]any{"includeThoughts": false, "thinkingLevel": "LOW"}},
		{name: "disabled invalid level", model: "gemini-4-argon", opts: []CallOption{WithThinkingDisabled(), WithThinkingLevel("bad")}, errText: "thinking level"},
		{name: "trailing slash", model: "gemini-4-argon/", opts: []CallOption{WithThinkingLevel("LOW")}, errText: "thinking level"},
		{name: "zero budget pointer", model: "gemini-4-argon", opts: []CallOption{func(o *CallOptions) { v := 0; o.ThinkingBudget = &v }}, errText: "thinking budget"},
		{name: "retained budget", model: "gemini-4-argon", opts: []CallOption{WithThinking(true, 10), WithThinkingDisabled()}, errText: "thinking budget"},
		{name: "budget alone ignored", model: "gemini-2.5-flash", opts: []CallOption{func(o *CallOptions) { v := 100; o.ThinkingBudget = &v }}},
	}
	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tt.name, stream), func(t *testing.T) {
				var calls atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var body struct {
						Generation map[string]any `json:"generation_config"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					got, _ := body.Generation["thinkingConfig"].(map[string]any)
					if !reflect.DeepEqual(got, tt.want) {
						t.Errorf("thinkingConfig = %#v, want %#v", got, tt.want)
					}
					if body.Generation["temperature"] != 0.5 || body.Generation["topP"] != 0.75 {
						t.Errorf("sampling options lost: %v", body.Generation)
					}
					if body.Generation["maxOutputTokens"] != float64(100) {
						t.Errorf("existing max tokens lost: %v", body.Generation)
					}
					response := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: %s\n\n", response)
					} else {
						fmt.Fprint(w, response)
					}
				}))
				defer srv.Close()
				m, err := NewGeminiChatModel(GeminiConfig{APIKey: "test", BaseURL: srv.URL, Model: tt.model})
				if err != nil {
					t.Fatal(err)
				}
				opts := append([]CallOption{WithMaxTokens(100), WithTemperature(0.5), WithTopP(0.75)}, tt.opts...)
				msgs := []*message.Msg{message.NewMsg("user", message.RoleUser, "hi")}
				if stream {
					var ch <-chan ChatResponse
					ch, err = m.ChatStream(context.Background(), msgs, opts...)
					if err == nil {
						for chunk := range ch {
							if chunk.Error != nil {
								t.Error(chunk.Error)
							}
						}
					}
				} else {
					_, err = m.Chat(context.Background(), msgs, opts...)
				}
				if tt.errText != "" {
					if err == nil || !strings.Contains(err.Error(), tt.errText) {
						t.Fatalf("error = %v, want %q", err, tt.errText)
					}
					if calls.Load() != 0 {
						t.Fatal("invalid options dispatched HTTP request")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if calls.Load() != 1 {
						t.Fatalf("calls = %d", calls.Load())
					}
				}
			})
		}
	}
}

func TestWithThinkingLevel(t *testing.T) {
	var opts CallOptions
	WithThinkingLevel("mEdIuM")(&opts)
	if opts.ThinkingLevel == nil || *opts.ThinkingLevel != "MEDIUM" {
		t.Fatalf("level = %v", opts.ThinkingLevel)
	}
	if opts.ThinkingEnable != nil || opts.ThinkingBudget != nil {
		t.Fatal("level option changed enable or budget")
	}
}
