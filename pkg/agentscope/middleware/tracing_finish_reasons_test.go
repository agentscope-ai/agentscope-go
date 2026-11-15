package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func findFinishReasons(t *testing.T, tracer *attributedRecordingTracer) string {
	t.Helper()
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	for _, s := range tracer.spans {
		if s.name != "chat.response" {
			continue
		}
		for _, a := range s.attrs {
			if a.Key == "gen_ai.response.finish_reasons" {
				v, _ := a.Value.(string)
				return v
			}
		}
	}
	t.Fatal("chat.response span with finish_reasons not found")
	return ""
}

// Upstream #2450: chat spans must record the real finish reason instead of
// a hardcoded "stop"; interruptions must be visible in traces.
//
// The value is a JSON array of strings, so it is asserted with valid JSON and
// re-parsed below to catch concatenation bugs.
func TestTracingFinishReasons(t *testing.T) {
	tests := []struct {
		name string
		resp *model.ChatResponse
		err  error
		want string
	}{
		{"plain stop", &model.ChatResponse{}, nil, `["stop"]`},
		{"length", &model.ChatResponse{StopReason: model.StopReasonLength}, nil, `["length"]`},
		// A partial reply (upstream #2350) and a failed call are different
		// outcomes and must stay distinguishable in traces; only a canceled
		// context is "interrupted".
		{"partial response is incomplete", &model.ChatResponse{Error: errors.New("boom")}, nil, `["incomplete"]`},
		{"handler error is error", &model.ChatResponse{}, errors.New("call failed"), `["error"]`},
		// StopReason comes from the provider: a quote or backslash must not
		// produce an invalid JSON attribute value.
		{"hostile stop reason stays valid json",
			&model.ChatResponse{StopReason: `a"b\c`}, nil, `["a\"b\\c"]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tracer := &attributedRecordingTracer{}
			mw := NewTracingMiddleware(tracer)
			next := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
				return tc.resp, tc.err
			}
			_, _ = mw.OnModelCall(context.Background(), &ModelCallInput{AgentName: "a"}, next)
			got := findFinishReasons(t, tracer)
			if got != tc.want {
				t.Errorf("finish_reasons = %s, want %s", got, tc.want)
			}
			var decoded []string
			if err := json.Unmarshal([]byte(got), &decoded); err != nil {
				t.Fatalf("finish_reasons %q is not valid JSON: %v", got, err)
			}
			if len(decoded) != 1 {
				t.Errorf("finish_reasons decoded to %v, want exactly one reason", decoded)
			}
		})
	}
}

func TestTracingFinishReasonsCanceledContext(t *testing.T) {
	tracer := &attributedRecordingTracer{}
	mw := NewTracingMiddleware(tracer)
	ctx, cancel := context.WithCancel(context.Background())
	next := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		cancel() // the call itself observes the cancellation
		return &model.ChatResponse{}, ctx.Err()
	}
	_, _ = mw.OnModelCall(ctx, &ModelCallInput{AgentName: "a"}, next)
	if got := findFinishReasons(t, tracer); got != `["interrupted"]` {
		t.Errorf("finish_reasons = %s, want interrupted", got)
	}
}

// A nil response keeps the established contract: no supplementary
// chat.response span is created (see tracing_test.go).
func TestTracingFinishReasonsNilResponseNoSpan(t *testing.T) {
	tracer := &attributedRecordingTracer{}
	mw := NewTracingMiddleware(tracer)
	next := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return nil, errors.New("gone")
	}
	_, _ = mw.OnModelCall(context.Background(), &ModelCallInput{AgentName: "a"}, next)
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	for _, s := range tracer.spans {
		if s.name == "chat.response" {
			t.Fatal("nil response must not create a chat.response span")
		}
	}
}
