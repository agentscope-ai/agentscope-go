package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// K3-series models always think: max_tokens maps to max_completion_tokens,
// fixed sampling parameters and the thinking switch stay off the wire, and
// the disable-thinking structured-output fallback offers nothing (upstream
// #3578). Other Moonshot models keep the existing wire shape.
func TestMoonshotK3RequestWire(t *testing.T) {
	k3 := map[string]bool{
		"kimi-k3": true, "kimi-k3.1-pro": true, "kimi-k3.1-pro-highspeed": true,
	}
	for _, name := range []string{"kimi-k3", "kimi-k3.1-pro", "kimi-k3.1-pro-highspeed", "kimi-k2.6", "moonshot-v1-8k"} {
		for _, stream := range []bool{false, true} {
			t.Run(name+"/"+fmt.Sprint(stream), func(t *testing.T) {
				var captured map[string]any
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
						t.Error(err)
						http.Error(w, "decode", 400)
						return
					}
					if stream {
						fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					} else {
						fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
					}
				}))
				defer srv.Close()
				m, err := NewMoonshotChatModel(MoonshotConfig{APIKey: "test", Model: name, BaseURL: srv.URL})
				if err != nil {
					t.Fatal(err)
				}
				msgs := []*message.Msg{message.UserMsg("u", "hi")}
				opts := []CallOption{
					WithTemperature(0.7), WithMaxTokens(100), WithTopP(0.9),
					WithSeed(42), WithThinkingDisabled(),
				}
				if stream {
					ch, e := m.ChatStream(context.Background(), msgs, opts...)
					if e != nil {
						t.Fatal(e)
					}
					for r := range ch {
						if r.Error != nil {
							t.Fatal(r.Error)
						}
					}
				} else if _, err = m.Chat(context.Background(), msgs, opts...); err != nil {
					t.Fatal(err)
				}
				if k3[name] {
					if _, ok := captured["max_tokens"]; ok {
						t.Errorf("max_tokens on wire: %v", captured)
					}
					if v := captured["max_completion_tokens"]; v != float64(100) {
						t.Errorf("max_completion_tokens = %v, want 100", v)
					}
					for _, k := range []string{"temperature", "top_p", "thinking"} {
						if _, ok := captured[k]; ok {
							t.Errorf("%s on wire: %v", k, captured)
						}
					}
					if v := captured["seed"]; v != float64(42) {
						t.Errorf("seed = %v, want 42", v)
					}
				} else {
					if v := captured["max_tokens"]; v != float64(100) {
						t.Errorf("max_tokens = %v, want 100", v)
					}
					if _, ok := captured["max_completion_tokens"]; ok {
						t.Errorf("max_completion_tokens on wire: %v", captured)
					}
					for _, k := range []string{"temperature", "top_p", "seed"} {
						if _, ok := captured[k]; !ok {
							t.Errorf("%s missing: %v", k, captured)
						}
					}
					thinking, ok := captured["thinking"].(map[string]any)
					if !ok || thinking["type"] != "disabled" {
						t.Errorf("thinking = %v, want disabled", captured["thinking"])
					}
				}
			})
		}
	}
}

func TestMoonshotK3DisableThinkingOptions(t *testing.T) {
	for _, name := range []string{"kimi-k3", "kimi-k3.1-pro", "kimi-k3.1-pro-highspeed"} {
		m, err := NewMoonshotChatModel(MoonshotConfig{APIKey: "test", Model: name})
		if err != nil {
			t.Fatal(err)
		}
		if opts := m.DisableThinkingOptions(); len(opts) != 0 {
			t.Errorf("%s: disable options = %v, want none", name, opts)
		}
	}
	m, err := NewMoonshotChatModel(MoonshotConfig{APIKey: "test", Model: "kimi-k2.6"})
	if err != nil {
		t.Fatal(err)
	}
	if opts := m.DisableThinkingOptions(); len(opts) != 1 {
		t.Errorf("legacy disable options = %v, want one", opts)
	}
}
