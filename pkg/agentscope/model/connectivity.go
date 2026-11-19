package model

import (
	"context"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/sirupsen/logrus"
)

// connectivityState tracks cloud availability for the ConnectivityAwareModel.
type connectivityState int

const (
	connClosed   connectivityState = iota // cloud available
	connOpen                              // cloud unavailable
	connHalfOpen                          // probing cloud
)

// connectivityBreaker is a minimal circuit breaker embedded in the model package
// to avoid an import cycle with the resilience package. It implements the same
// single-probe half-open algorithm as resilience.CircuitBreaker.
type connectivityBreaker struct {
	threshold    int
	resetTimeout time.Duration

	mu              sync.Mutex
	state           connectivityState
	failures        int
	lastFailure     time.Time
	halfOpenProbing bool
}

func newConnectivityBreaker(threshold int, resetTimeout time.Duration) *connectivityBreaker {
	if threshold < 1 {
		threshold = 1
	}
	return &connectivityBreaker{
		threshold:    threshold,
		resetTimeout: resetTimeout,
		state:        connClosed,
	}
}

func (cb *connectivityBreaker) getState() connectivityState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.state == connOpen && time.Since(cb.lastFailure) >= cb.resetTimeout {
		cb.state = connHalfOpen
	}
	return cb.state
}

// execute runs fn if the breaker permits. Returns errCircuitOpen if rejected.
func (cb *connectivityBreaker) execute(fn func() error) error {
	if err := cb.beforeCall(); err != nil {
		return err
	}
	err := fn()
	cb.afterCall(err)
	return err
}

var errCircuitOpen = &circuitOpenError{}

type circuitOpenError struct{}

func (e *circuitOpenError) Error() string { return "connectivity circuit breaker is open" }

func (cb *connectivityBreaker) beforeCall() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case connOpen:
		if time.Since(cb.lastFailure) >= cb.resetTimeout {
			cb.state = connHalfOpen
			cb.halfOpenProbing = true
			return nil
		}
		return errCircuitOpen
	case connHalfOpen:
		if cb.halfOpenProbing {
			return errCircuitOpen
		}
		cb.halfOpenProbing = true
		return nil
	}
	return nil
}

func (cb *connectivityBreaker) afterCall(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if err != nil {
		cb.failures++
		cb.lastFailure = time.Now()
		if cb.state == connHalfOpen || cb.failures >= cb.threshold {
			cb.state = connOpen
		}
		cb.halfOpenProbing = false
		return
	}

	// Success: recover fully.
	cb.failures = 0
	cb.state = connClosed
	cb.halfOpenProbing = false
}

// ConnectivityAwareModel wraps two ChatModel implementations (local and cloud)
// and routes calls based on network connectivity state. It uses an internal
// circuit breaker to track cloud availability: when the breaker is closed, calls
// go to the cloud model; when open, calls route to the local model; in half-open
// state, a single probe call goes to the cloud to test recovery.
//
// This is designed for edge/IoT deployments where a local model (e.g. Ollama on
// localhost) is always available but a cloud model should be preferred when
// connectivity permits.
type ConnectivityAwareModel struct {
	local     ChatModel
	cloud     ChatModel
	cb        *connectivityBreaker
	canonical bool
}

// ConnectivityOption configures a ConnectivityAwareModel.
type ConnectivityOption func(*connectivityConfig)

type connectivityConfig struct {
	failureThreshold int
	recoveryTimeout  time.Duration
	canonicalReplay  bool
}

// WithFailureThreshold sets the number of consecutive cloud failures before
// switching to the local model. Default is 3.
func WithFailureThreshold(n int) ConnectivityOption {
	return func(c *connectivityConfig) {
		if n > 0 {
			c.failureThreshold = n
		}
	}
}

// WithRecoveryTimeout sets how long to wait before probing the cloud model
// again after the circuit opens. Default is 30s.
func WithRecoveryTimeout(d time.Duration) ConnectivityOption {
	return func(c *connectivityConfig) {
		if d > 0 {
			c.recoveryTimeout = d
		}
	}
}

// WithCanonicalReplay (EXPERIMENTAL) sends the cloud model the arguments of
// the local model's tool calls as compact JSON with sorted keys. The local
// model's calls are marked when they pass through (ToolCallBlock.Extra), and
// the rewrite applies to the copy of the history sent to the cloud: the
// stored messages, the local model's requests and the cloud's own calls are
// unchanged. The rewrite is deterministic, so cached prefixes stay stable.
// Calls written before the option was enabled carry no mark, and arguments
// that are not exactly one JSON value have no canonical form; both are sent
// as written.
//
// In one measurement, a cloud model sent a local model's write_file calls
// with path before content ran into an 8,000-token output cap on 6 to 8% of
// its replies, and on none with the keys sorted. See
// docs/edge-deployment.md for that measurement and its limits: one cloud
// model, one local model and one tool.
func WithCanonicalReplay() ConnectivityOption {
	return func(c *connectivityConfig) { c.canonicalReplay = true }
}

// NewConnectivityAwareModel creates a connectivity-aware model that routes
// between a local fallback and a cloud model based on circuit breaker state.
func NewConnectivityAwareModel(local, cloud ChatModel, opts ...ConnectivityOption) *ConnectivityAwareModel {
	cfg := &connectivityConfig{
		failureThreshold: 3,
		recoveryTimeout:  30 * time.Second,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	return &ConnectivityAwareModel{
		local:     local,
		cloud:     cloud,
		cb:        newConnectivityBreaker(cfg.failureThreshold, cfg.recoveryTimeout),
		canonical: cfg.canonicalReplay,
	}
}

// Chat routes the call to the cloud or local model based on circuit breaker state.
func (m *ConnectivityAwareModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	state := m.cb.getState()

	switch state {
	case connOpen:
		// Cloud is known-unavailable; use local directly.
		logrus.Debug("connectivity: circuit open, routing to local model")
		return m.chatLocal(ctx, msgs, opts)

	case connClosed, connHalfOpen:
		// Try cloud via circuit breaker.
		var resp *ChatResponse
		err := m.cb.execute(func() error {
			var callErr error
			resp, callErr = m.cloud.Chat(ctx, m.cloudMsgs(msgs), opts...)
			return callErr
		})

		if err == nil {
			return resp, nil
		}

		// If context was canceled, don't fall back.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Circuit breaker rejected or cloud call failed.
		logrus.WithError(err).Warn("connectivity: cloud model failed, falling back to local")
		return m.chatLocal(ctx, msgs, opts)
	}

	// Unreachable, but satisfy the compiler.
	return m.chatLocal(ctx, msgs, opts)
}

// ChatStream routes streaming calls based on circuit breaker state.
func (m *ConnectivityAwareModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	state := m.cb.getState()

	switch state {
	case connOpen:
		logrus.Debug("connectivity: circuit open, streaming from local model")
		return m.streamLocal(ctx, msgs, opts)

	case connClosed, connHalfOpen:
		var ch <-chan ChatResponse
		err := m.cb.execute(func() error {
			var callErr error
			ch, callErr = m.cloud.ChatStream(ctx, m.cloudMsgs(msgs), opts...)
			return callErr
		})

		if err == nil {
			return ch, nil
		}

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		logrus.WithError(err).Warn("connectivity: cloud stream failed, falling back to local")
		return m.streamLocal(ctx, msgs, opts)
	}

	return m.streamLocal(ctx, msgs, opts)
}

// cloudMsgs is the history the cloud model receives (WithCanonicalReplay).
func (m *ConnectivityAwareModel) cloudMsgs(msgs []*message.Msg) []*message.Msg {
	if m.canonical {
		return canonicalFor(msgs, 0)
	}
	return msgs
}

// chatLocal calls the local model and, with canonical replay, marks its
// tool calls.
func (m *ConnectivityAwareModel) chatLocal(ctx context.Context, msgs []*message.Msg, opts []CallOption) (*ChatResponse, error) {
	resp, err := m.local.Chat(ctx, msgs, opts...)
	if m.canonical {
		resp = markWriter(resp, 1)
	}
	return resp, err
}

// streamLocal streams from the local model and, with canonical replay,
// marks the tool calls of its final response.
func (m *ConnectivityAwareModel) streamLocal(ctx context.Context, msgs []*message.Msg, opts []CallOption) (<-chan ChatResponse, error) {
	ch, err := m.local.ChatStream(ctx, msgs, opts...)
	if m.canonical && err == nil && ch != nil {
		ch = markWriterStream(ctx, ch, 1)
	}
	return ch, err
}

// CountTokens delegates to whichever model would currently be selected.
func (m *ConnectivityAwareModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	if m.cb.getState() == connOpen {
		return m.local.CountTokens(msgs, tools)
	}
	return m.cloud.CountTokens(msgs, tools)
}

// ActiveModel returns which model is currently active: "cloud" or "local".
func (m *ConnectivityAwareModel) ActiveModel() string {
	if m.cb.getState() == connOpen {
		return "local"
	}
	return "cloud"
}

// Compile-time interface check.
var _ ChatModel = (*ConnectivityAwareModel)(nil)
