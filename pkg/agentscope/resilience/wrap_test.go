package resilience

import (
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// testContextFallback mirrors the agent's default window for models that
// report none.
const testContextFallback = 128000

func ollamaModel(t *testing.T, contextSize int) model.ChatModel {
	t.Helper()
	m, err := model.NewOllamaChatModel(model.OllamaConfig{Model: "qwen2.5:0.5b", ContextSize: contextSize})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func resilienceOptions() map[string][]ResilienceOption {
	return map[string][]ResilienceOption{
		"circuit breaker": {WithCircuitBreaker(NewCircuitBreaker(3, time.Minute))},
		"rate limiter":    {WithRateLimit(NewRateLimiter(10, 1))},
		"both":            {WithCircuitBreaker(NewCircuitBreaker(3, time.Minute)), WithRateLimit(NewRateLimiter(10, 1))},
	}
}

func TestWrap_ResolvesInnerContextSize(t *testing.T) {
	for name, opts := range resilienceOptions() {
		t.Run(name, func(t *testing.T) {
			inner := ollamaModel(t, 8192)
			w := Wrap(inner, opts...)
			u, ok := w.(model.ChatModelUnwrapper)
			if !ok {
				t.Fatalf("Wrap returned %T, which does not implement model.ChatModelUnwrapper", w)
			}
			if got := u.Unwrap(); got != inner {
				t.Errorf("Unwrap = %T %p, want the wrapped model %p", got, got, inner)
			}
			if got := model.ResolveContextSize(w, testContextFallback); got != 8192 {
				t.Errorf("ResolveContextSize = %d, want the wrapped model's 8192", got)
			}
		})
	}
}

func TestWrap_ConnectivityAwareModelKeepsLocalWindow(t *testing.T) {
	cam := model.NewConnectivityAwareModel(ollamaModel(t, 8192), ollamaModel(t, 0))
	w := Wrap(cam, WithCircuitBreaker(NewCircuitBreaker(3, time.Minute)))
	if got := model.ResolveContextSize(w, testContextFallback); got != 8192 {
		t.Errorf("ResolveContextSize = %d, want the local model's 8192", got)
	}
}

func TestWrap_NoOptionsReturnsModel(t *testing.T) {
	inner := ollamaModel(t, 8192)
	if got := Wrap(inner); got != inner {
		t.Errorf("Wrap without options = %T %p, want the model itself %p", got, got, inner)
	}
}

func TestWrap_NilModelResolvesToFallback(t *testing.T) {
	w := Wrap(nil, WithCircuitBreaker(NewCircuitBreaker(3, time.Minute)))
	if got := model.ResolveContextSize(w, testContextFallback); got != testContextFallback {
		t.Errorf("ResolveContextSize = %d, want fallback %d", got, testContextFallback)
	}
}
