package middleware

import (
	"context"
	"fmt"
	"math"
	"sync"
)

// ReplyBudgetCounter is the owned checkpoint state of a built-in reply budget.
// Custom middleware state is not serialized by this protocol. These are observed
// successful-call budgets, not reservations for concurrent/in-flight spending.
type ReplyBudgetCounter struct {
	UsedTokens float64 `json:"used_tokens"`
	SpentUSD   float64 `json:"spent_usd"`
	Warned     bool    `json:"warned"`
}

// ReplyBudgetSnapshot has a separate version from AgentState. Unknown versions,
// negative/nonfinite counters and excessive namespaces are rejected on resume.
type ReplyBudgetSnapshot struct {
	Version  int                           `json:"version"`
	Counters map[string]ReplyBudgetCounter `json:"counters"`
}
type replyBudgetState struct {
	mu       sync.Mutex
	counters map[string]ReplyBudgetCounter
}
type replyBudgetKey struct{}

// WithReplyBudgetSnapshot attaches synchronized budget state for this logical
// reply. Nil starts fresh counters; a non-nil snapshot resumes counters without
// OnReply resetting them. Ordinary middleware still uses MiddleContext. Only
// the built-in token and USD reply budgets opt into this typed state bridge.
func WithReplyBudgetSnapshot(ctx context.Context, snapshot *ReplyBudgetSnapshot) (context.Context, error) {
	if GetMiddleContext(ctx) == nil {
		ctx = WithMiddleContext(ctx, MiddleContext{})
	}
	state := &replyBudgetState{counters: make(map[string]ReplyBudgetCounter)}
	if snapshot != nil {
		if snapshot.Version != 1 || len(snapshot.Counters) > 128 {
			return nil, fmt.Errorf("middleware: unsupported or oversized reply budget snapshot")
		}
		for key, value := range snapshot.Counters {
			if key == "" || len(key) > 256 || !validBudgetCounter(value) {
				return nil, fmt.Errorf("middleware: invalid reply budget counter")
			}
			state.counters[key] = value
		}
	}
	return context.WithValue(ctx, replyBudgetKey{}, state), nil
}
func validBudgetCounter(c ReplyBudgetCounter) bool {
	return c.UsedTokens >= 0 && c.SpentUSD >= 0 && !math.IsNaN(c.UsedTokens) && !math.IsNaN(c.SpentUSD) && !math.IsInf(c.UsedTokens, 0) && !math.IsInf(c.SpentUSD, 0)
}
func budgetState(ctx context.Context) *replyBudgetState {
	s, _ := ctx.Value(replyBudgetKey{}).(*replyBudgetState)
	return s
}

// SnapshotReplyBudgets returns a detached snapshot, or nil for a legacy reply.
func SnapshotReplyBudgets(ctx context.Context) *ReplyBudgetSnapshot {
	state := budgetState(ctx)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	snapshot := &ReplyBudgetSnapshot{Version: 1, Counters: make(map[string]ReplyBudgetCounter, len(state.counters))}
	for key, value := range state.counters {
		snapshot.Counters[key] = value
	}
	return snapshot
}
func (s *replyBudgetState) get(key string) ReplyBudgetCounter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counters[key]
}
func (s *replyBudgetState) update(key string, change func(*ReplyBudgetCounter)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.counters[key]
	change(&value)
	s.counters[key] = value
}
func (m *ReplyBudgetControlMiddleware) usedInContext(ctx context.Context, mc MiddleContext) float64 {
	if state := budgetState(ctx); state != nil {
		return state.get(m.Key()).UsedTokens
	}
	return m.getUsed(mc)
}
func (m *ReplyBudgetControlMiddleware) addInContext(ctx context.Context, mc MiddleContext, cost float64) {
	if state := budgetState(ctx); state != nil {
		state.update(m.Key(), func(c *ReplyBudgetCounter) { c.UsedTokens += cost })
		return
	}
	m.addUsed(mc, cost)
}
func (m *ReplyCostBudgetMiddleware) spentInContext(ctx context.Context, mc MiddleContext) float64 {
	if state := budgetState(ctx); state != nil {
		return state.get(m.Key()).SpentUSD
	}
	return m.spent(mc)
}
func (m *ReplyCostBudgetMiddleware) warnedInContext(ctx context.Context, mc MiddleContext) bool {
	if state := budgetState(ctx); state != nil {
		return state.get(m.Key()).Warned
	}
	v, _ := mc.Get(m.Key(), "warned")
	return v == true
}
func (m *ReplyCostBudgetMiddleware) recordInContext(ctx context.Context, mc MiddleContext, delta float64) {
	if state := budgetState(ctx); state != nil {
		state.update(m.Key(), func(c *ReplyBudgetCounter) {
			c.SpentUSD += delta
			c.Warned = c.Warned || c.SpentUSD >= m.maxUSD*m.warnRatio
		})
		return
	}
	spent := m.spent(mc) + delta
	mc.Set(m.Key(), "spent", spent)
	if spent >= m.maxUSD*m.warnRatio {
		mc.Set(m.Key(), "warned", true)
	}
}
