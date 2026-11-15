package runtime

import (
	"sync"
	"sync/atomic"
	"time"

	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

// Budget defines resource limits for a runtime session.
type Budget struct {
	MaxTurns       int
	MaxTokens      int
	MaxDuration    time.Duration
	MaxConcurrency int
}

// BudgetTracker tracks resource consumption against a Budget.
// All methods are safe for concurrent use.
type BudgetTracker struct {
	budget    Budget
	turns     atomic.Int64
	tokens    atomic.Int64
	agents    atomic.Int64
	startOnce sync.Once
	mu        sync.Mutex
	startTime time.Time
}

// NewBudgetTracker creates a new BudgetTracker with the given budget limits.
// Zero values in Budget mean unlimited for that dimension.
func NewBudgetTracker(b Budget) *BudgetTracker {
	return &BudgetTracker{budget: b}
}

func (bt *BudgetTracker) start() {
	bt.startOnce.Do(func() {
		bt.mu.Lock()
		bt.startTime = time.Now()
		bt.mu.Unlock()
	})
}

// AddTurn records one turn of agent activity. Returns ErrBudgetExceeded if
// the turn limit has been reached. The turn counter is not incremented on
// failure.
func (bt *BudgetTracker) AddTurn() error {
	bt.start()
	n := bt.turns.Add(1)
	if bt.budget.MaxTurns > 0 && int(n) > bt.budget.MaxTurns {
		bt.turns.Add(-1)
		return agenterrors.ErrBudgetExceeded
	}
	return nil
}

// AddTokens records token consumption. Returns ErrBudgetExceeded if the
// token limit has been exceeded. Unlike AddTurn, the tokens are still
// recorded (they have already been consumed by the model call).
func (bt *BudgetTracker) AddTokens(n int) error {
	bt.start()
	total := bt.tokens.Add(int64(n))
	if bt.budget.MaxTokens > 0 && int(total) > bt.budget.MaxTokens {
		return agenterrors.ErrBudgetExceeded
	}
	return nil
}

// AcquireAgent increments the active agent count. Returns ErrBudgetExceeded
// if the concurrency limit has been reached. The count is not incremented on
// failure.
func (bt *BudgetTracker) AcquireAgent() error {
	bt.start()
	n := bt.agents.Add(1)
	if bt.budget.MaxConcurrency > 0 && int(n) > bt.budget.MaxConcurrency {
		bt.agents.Add(-1)
		return agenterrors.ErrBudgetExceeded
	}
	return nil
}

// ReleaseAgent decrements the active agent count.
func (bt *BudgetTracker) ReleaseAgent() {
	bt.agents.Add(-1)
}

// Exceeded returns true if any budget dimension has been reached or exceeded.
func (bt *BudgetTracker) Exceeded() bool {
	if bt.budget.MaxTurns > 0 && int(bt.turns.Load()) >= bt.budget.MaxTurns {
		return true
	}
	if bt.budget.MaxTokens > 0 && int(bt.tokens.Load()) >= bt.budget.MaxTokens {
		return true
	}
	if bt.budget.MaxDuration > 0 {
		bt.mu.Lock()
		st := bt.startTime
		bt.mu.Unlock()
		if !st.IsZero() && time.Since(st) >= bt.budget.MaxDuration {
			return true
		}
	}
	return false
}

// TurnsUsed returns the number of turns consumed.
func (bt *BudgetTracker) TurnsUsed() int {
	return int(bt.turns.Load())
}

// TokensUsed returns the total tokens consumed.
func (bt *BudgetTracker) TokensUsed() int {
	return int(bt.tokens.Load())
}

// ActiveAgents returns the number of currently active agents.
func (bt *BudgetTracker) ActiveAgents() int {
	return int(bt.agents.Load())
}

// Elapsed returns the time since the tracker was first used.
func (bt *BudgetTracker) Elapsed() time.Duration {
	bt.mu.Lock()
	st := bt.startTime
	bt.mu.Unlock()
	if st.IsZero() {
		return 0
	}
	return time.Since(st)
}

// Reset clears all counters and allows the tracker to be reused.
func (bt *BudgetTracker) Reset() {
	bt.turns.Store(0)
	bt.tokens.Store(0)
	bt.agents.Store(0)
	bt.mu.Lock()
	bt.startOnce = sync.Once{}
	bt.startTime = time.Time{}
	bt.mu.Unlock()
}
