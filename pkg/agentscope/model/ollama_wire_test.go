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

// ollamaExchange makes one Chat or ChatStream call with msgs (a single user
// message when nil) against a fixture that answers with reply, and returns
// the decoded request body and every response the adapter produced.
func ollamaExchange(t *testing.T, streaming bool, reply string, msgs []*message.Msg, opts ...CallOption) (map[string]any, []ChatResponse) {
	t.Helper()
	captured := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		select {
		case captured <- body:
		default:
			t.Error("adapter sent more than one request")
		}
		if streaming {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		fmt.Fprint(w, reply)
	}))
	defer srv.Close()

	m, err := NewOllamaChatModel(OllamaConfig{Model: "qwen3:14b", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if msgs == nil {
		msgs = []*message.Msg{message.UserMsg("user", "hello")}
	}

	var responses []ChatResponse
	if streaming {
		ch, err := m.ChatStream(ctx, msgs, opts...)
		if err != nil {
			t.Fatal(err)
		}
		for resp := range ch {
			if resp.Error != nil {
				t.Fatalf("stream error: %v", resp.Error)
			}
			responses = append(responses, resp)
		}
	} else {
		resp, err := m.Chat(ctx, msgs, opts...)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, *resp)
	}
	return <-captured, responses
}

func TestOllamaSeedOnWire(t *testing.T) {
	cases := []struct {
		name string
		opts []CallOption
		want any // nil: the key must be absent
	}{
		{"set", []CallOption{WithSeed(42)}, float64(42)},
		{"zero", []CallOption{WithSeed(0)}, float64(0)},
		{"unset", nil, nil},
	}
	for _, streaming := range []bool{false, true} {
		reply := openAIResponseJSON()
		if streaming {
			reply = openAIStreamResponseSSE()
		}
		for _, tc := range cases {
			t.Run(fmt.Sprintf("stream=%v/%s", streaming, tc.name), func(t *testing.T) {
				body, _ := ollamaExchange(t, streaming, reply, nil, tc.opts...)
				got, present := body["seed"]
				if tc.want == nil {
					if present {
						t.Errorf("seed = %v, want the key absent", got)
					}
					return
				}
				if got != tc.want {
					t.Errorf("seed = %v (present=%v), want %v", got, present, tc.want)
				}
			})
		}
	}
}
