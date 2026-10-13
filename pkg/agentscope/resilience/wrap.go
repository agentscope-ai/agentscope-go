package resilience

import (
	"context"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// ResilienceOption configures a resilient model wrapper.
type ResilienceOption func(*resilientModel)

// WithCircuitBreaker wraps model calls with the given circuit breaker.
func WithCircuitBreaker(cb *CircuitBreaker) ResilienceOption {
	return func(m *resilientModel) {
		m.cb = cb
	}
}

// WithRateLimit applies the given rate limiter before each model call.
func WithRateLimit(rl *RateLimiter) ResilienceOption {
	return func(m *resilientModel) {
		m.rl = rl
	}
}

// resilientModel decorates a model.ChatModel with rate limiting and a circuit
// breaker. Rate limiting is applied before the call; the circuit breaker wraps
// the call itself.
type resilientModel struct {
	inner model.ChatModel
	cb    *CircuitBreaker
	rl    *RateLimiter
}

// Wrap decorates m with the configured resilience options and returns a
// model.ChatModel. With no options it returns m unchanged. The wrapper
// implements model.ChatModelUnwrapper, so model.ResolveContextSize reports the
// window of m. Calling the unwrapped model directly bypasses the circuit
// breaker and rate limiter.
func Wrap(m model.ChatModel, opts ...ResilienceOption) model.ChatModel {
	rm := &resilientModel{inner: m}
	for _, opt := range opts {
		opt(rm)
	}
	if rm.cb == nil && rm.rl == nil {
		return m
	}
	return rm
}

// waitRate applies the rate limiter if configured.
func (m *resilientModel) waitRate(ctx context.Context) error {
	if m.rl == nil {
		return nil
	}
	return m.rl.Wait(ctx)
}

// guard runs fn through the circuit breaker if configured, otherwise directly.
func (m *resilientModel) guard(fn func() error) error {
	if m.cb == nil {
		return fn()
	}
	return m.cb.Execute(fn)
}

func (m *resilientModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	if err := m.waitRate(ctx); err != nil {
		return nil, err
	}
	var resp *model.ChatResponse
	err := m.guard(func() error {
		var callErr error
		resp, callErr = m.inner.Chat(ctx, msgs, opts...)
		return callErr
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (m *resilientModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	if err := m.waitRate(ctx); err != nil {
		return nil, err
	}
	var ch <-chan model.ChatResponse
	err := m.guard(func() error {
		var callErr error
		ch, callErr = m.inner.ChatStream(ctx, msgs, opts...)
		return callErr
	})
	if err != nil {
		return nil, err
	}
	return ch, nil
}

func (m *resilientModel) CountTokens(msgs []*message.Msg, tools []model.ToolSchema) int {
	return m.inner.CountTokens(msgs, tools)
}

// Unwrap returns the wrapped model.
func (m *resilientModel) Unwrap() model.ChatModel {
	return m.inner
}
