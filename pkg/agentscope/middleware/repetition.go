package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/audit"
	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// RepetitionBreakerMiddleware stops an agent from spinning on identical
// tool calls (HARNESS_DESIGN E1) — a common cost blowout shape. The call
// signature key includes the tool name and input; only successful calls
// count (a failed call legitimately may be retried and resets the streak).
// At the threshold a system-prompt hint is injected asking the model to
// change strategy; the identical call past the threshold still executes
// (middleware cannot un-run side effects) but its result is discarded —
// the typed ErrToolRepetition becomes the tool result the model sees on
// the next turn.
//
// Known limitation: spins whose inputs vary by timestamps/random values are
// not detected (hash-based matching).
//
// Streak state lives on the middleware instance guarded by a mutex because
// OnActing runs concurrently for parallel tool batches — MiddleContext is
// not synchronized and must not be touched from concurrent hooks. Streaks
// are keyed per (agent, reply) via the reply ID attached to ctx
// (audit.WithReplyID) so concurrent replies of one agent do not reset or
// contaminate each other (HARNESS review L-3).
type RepetitionBreakerMiddleware struct {
	BaseMiddleware
	threshold int
	hint      string
	allowlist map[string]bool

	// Error-streak dimension (upstream #1816): the SAME call failing
	// repeatedly is a distinct spin shape from identical successes.
	errThreshold int
	errHint      string
	errStreak    map[string]repetitionStreak

	mu     sync.Mutex
	streak map[string]repetitionStreak // per agent name + reply ID
}

type repetitionStreak struct {
	lastKey string
	count   int
}

const defaultRepetitionHint = "<system-reminder>You have repeated the exact same tool call several times without progress. Stop retrying it and try a different approach, or report that the task cannot be completed.</system-reminder>"

const defaultRepetitionErrorHint = "<system-reminder>The exact same tool call has failed repeatedly. Do not retry it unchanged: fix the arguments, use a different tool, or report that the task cannot be completed.</system-reminder>"

// RepetitionBreakerOption configures NewRepetitionBreaker.
type RepetitionBreakerOption func(*RepetitionBreakerMiddleware)

// WithRepetitionThreshold sets how many consecutive identical successful
// calls trigger the hint (default 3). The call after the hint is aborted.
func WithRepetitionThreshold(n int) RepetitionBreakerOption {
	return func(m *RepetitionBreakerMiddleware) {
		if n > 0 {
			m.threshold = n
		}
	}
}

// WithRepetitionHint overrides the injected reminder text.
func WithRepetitionHint(hint string) RepetitionBreakerOption {
	return func(m *RepetitionBreakerMiddleware) {
		if hint != "" {
			m.hint = hint
		}
	}
}

// WithRepetitionAllowlist exempts tools (by name) from repetition breaking —
// use for read-only/idempotent tools that legitimately repeat.
func WithRepetitionAllowlist(names ...string) RepetitionBreakerOption {
	return func(m *RepetitionBreakerMiddleware) {
		for _, n := range names {
			m.allowlist[n] = true
		}
	}
}

// WithRepetitionErrorThreshold sets how many consecutive identical FAILING
// calls trigger the error hint (default 3); the call after that is aborted
// (upstream #1816).
func WithRepetitionErrorThreshold(n int) RepetitionBreakerOption {
	return func(m *RepetitionBreakerMiddleware) {
		if n > 0 {
			m.errThreshold = n
		}
	}
}

// WithRepetitionErrorHint overrides the repeated-failure reminder text.
func WithRepetitionErrorHint(hint string) RepetitionBreakerOption {
	return func(m *RepetitionBreakerMiddleware) {
		if hint != "" {
			m.errHint = hint
		}
	}
}

// NewRepetitionBreaker creates the middleware with defaults (threshold 3).
func NewRepetitionBreaker(opts ...RepetitionBreakerOption) *RepetitionBreakerMiddleware {
	m := &RepetitionBreakerMiddleware{
		BaseMiddleware: BaseMiddleware{MiddlewareKey: "repetition-breaker"},
		threshold:      3,
		hint:           defaultRepetitionHint,
		errThreshold:   3,
		errHint:        defaultRepetitionErrorHint,
		allowlist:      map[string]bool{},
		streak:         map[string]repetitionStreak{},
		errStreak:      map[string]repetitionStreak{},
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// streakMapLimit bounds the per-reply streak map on long-lived middleware
// instances: one entry per reply would otherwise accumulate unboundedly.
// Overflow drops all streaks at once — repetition detection is advisory,
// and the cap only trips after many thousands of replies on one instance.
const streakMapLimit = 8192

// OnReply resets the fallback (reply-ID-less) streak key. In agent-attached
// flows the reply ID is minted inside the reply loop — after this hook —
// so per-reply isolation there comes from the keyed streaks themselves
// (each reply gets a fresh key); the reset below covers standalone use
// where ctx carries no reply ID. The map stays bounded via streakMapLimit.
func (m *RepetitionBreakerMiddleware) OnReply(ctx context.Context, input ReplyInput, next ReplyHandler) <-chan event.Event {
	key := repetitionStreakKey(ctx, input.AgentName)
	m.mu.Lock()
	if len(m.streak) >= streakMapLimit {
		m.streak = map[string]repetitionStreak{}
		m.errStreak = map[string]repetitionStreak{}
	}
	m.streak[key] = repetitionStreak{}
	m.errStreak[key] = repetitionStreak{}
	m.mu.Unlock()
	return next(ctx, input)
}

// OnActing tracks identical successful calls and breaks the loop.
func (m *RepetitionBreakerMiddleware) OnActing(ctx context.Context, input *ActingInput, next ActingHandler) (*tool.ToolResponse, error) {
	resp, err := next(ctx, input)

	if m.allowlist[input.ToolCall.Name] {
		return resp, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	streakKey := repetitionStreakKey(ctx, input.AgentName)
	key := repetitionKey(input.ToolCall.Name, input.ToolCall.Input)

	// Upstream #1816: a failed result (handler error OR error-state tool
	// response) feeds the error streak and resets the success streak — the
	// doc contract always said only successful calls count toward the spin
	// streak; error-state responses with a nil Go error used to slip in.
	failed := err != nil || (resp != nil && resp.State == message.ToolResultError)
	if failed {
		m.streak[streakKey] = repetitionStreak{}
		es := m.errStreak[streakKey]
		if es.lastKey == key {
			es.count++
		} else {
			es = repetitionStreak{lastKey: key, count: 1}
		}
		m.errStreak[streakKey] = es
		if es.count > m.errThreshold {
			return nil, agenterrors.ErrToolRepetition
		}
		return resp, err
	}

	m.errStreak[streakKey] = repetitionStreak{}
	st := m.streak[streakKey]
	if st.lastKey == key {
		st.count++
	} else {
		st = repetitionStreak{lastKey: key, count: 1}
	}
	m.streak[streakKey] = st

	if st.count > m.threshold {
		return nil, agenterrors.ErrToolRepetition
	}
	return resp, nil
}

// OnSystemPrompt injects the strategy-change hint once the streak reaches
// the threshold.
func (m *RepetitionBreakerMiddleware) OnSystemPrompt(ctx context.Context, agentName string, currentPrompt string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := repetitionStreakKey(ctx, agentName)
	if st, ok := m.streak[key]; ok && st.count >= m.threshold {
		return currentPrompt + "\n\n" + m.hint
	}
	// Upstream #1816: the error-streak hint is an independent dimension.
	if es, ok := m.errStreak[key]; ok && es.count >= m.errThreshold {
		return currentPrompt + "\n\n" + m.errHint
	}
	return currentPrompt
}

// repetitionStreakKey derives the per-reply streak key. The agent attaches
// the reply ID to ctx (audit.WithReplyID); without one (standalone use) the
// key falls back to the agent name.
func repetitionStreakKey(ctx context.Context, agentName string) string {
	if rid := audit.ReplyIDFromCtx(ctx); rid != "" {
		return agentName + "\x00" + rid
	}
	return agentName
}

func repetitionKey(name, input string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + input))
	return hex.EncodeToString(sum[:16])
}
