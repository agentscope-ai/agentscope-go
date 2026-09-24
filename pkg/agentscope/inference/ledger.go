package inference

import (
	"math"
	"sync"
	"time"
)

// Usage contains disjoint billing categories. InputTokens excludes cache reads
// and writes. UsageKnown on Attempt distinguishes a reported zero from absence.
type Usage struct {
	InputKnown       bool  `json:"input_known"`
	OutputKnown      bool  `json:"output_known"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// Attempt records one physical HTTP send attempt. It deliberately excludes
// prompts, request/response bodies, URLs, headers and provider error strings.
type Attempt struct {
	UsageComplete bool        `json:"usage_complete"`
	ID            uint64      `json:"id"`
	OperationID   uint64      `json:"operation_id"`
	Number        int         `json:"number"`
	TargetID      string      `json:"target_id"`
	PoolID        string      `json:"pool_id"`
	Provider      string      `json:"provider"`
	API           string      `json:"api"`
	Model         string      `json:"model"`
	Identity      Identity    `json:"identity"`
	Attribution   Attribution `json:"attribution"`
	Purpose       string      `json:"purpose"`
	StartedAt     time.Time   `json:"started_at"`
	FinishedAt    time.Time   `json:"finished_at,omitempty"`
	Status        int         `json:"status,omitempty"`
	Outcome       string      `json:"outcome"`
	Usage         Usage       `json:"usage"`
	UsageKnown    bool        `json:"usage_known"`
	CostUSD       float64     `json:"known_cost_usd"`
	CostKnown     bool        `json:"cost_known"`
}

// OperationRecord describes logical work. It has no additive token/cost totals:
// attempts are the sole physical cost source, including retries and failed work.
type OperationRecord struct {
	ID          uint64      `json:"id"`
	TargetID    string      `json:"target_id"`
	Model       string      `json:"model"`
	Identity    Identity    `json:"identity"`
	Attribution Attribution `json:"attribution"`
	Purpose     string      `json:"purpose"`
	StartedAt   time.Time   `json:"started_at"`
	FinishedAt  time.Time   `json:"finished_at,omitempty"`
	Outcome     string      `json:"outcome"`
}

// Snapshot is detached and versioned. KnownCostUSD is a subtotal. Incomplete is
// true for retention loss or unfinished operations/attempts. UnknownUsage and
// UnknownCost additionally expose completed requests without billing evidence.
// Dropped is ledger-wide, even in a filtered snapshot: lost attribution cannot
// safely be assigned to a run. Use a dedicated ledger for isolated experiments.
type Snapshot struct {
	CostOverflow bool              `json:"cost_overflow"`
	Version      int               `json:"version"`
	ObservedAt   time.Time         `json:"observed_at"`
	Attempts     []Attempt         `json:"attempts"`
	Operations   []OperationRecord `json:"operations"`
	Dropped      uint64            `json:"dropped"`
	Incomplete   bool              `json:"incomplete"`
	UnknownUsage int               `json:"unknown_usage"`
	UnknownCost  int               `json:"unknown_cost"`
	KnownCostUSD float64           `json:"known_cost_usd"`
}

// Ledger retains at most capacity attempts and capacity operations. Pending
// records participate in that bound; completion of an evicted record cannot
// resurrect it. A snapshot never changes after it is returned.
type Ledger struct {
	mu             sync.Mutex
	capacity       int
	sequence       uint64
	attempts       []Attempt
	operations     []OperationRecord
	attemptIndex   map[uint64]int
	operationIndex map[uint64]int
	attemptHead    int
	operationHead  int
	dropped        uint64
}

func NewLedger(capacity int) *Ledger {
	if capacity < 1 {
		capacity = 10000
	}
	return &Ledger{capacity: capacity, attemptIndex: make(map[uint64]int), operationIndex: make(map[uint64]int)}
}
func (l *Ledger) begin(a *Attempt) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sequence++
	a.ID = l.sequence
	index := len(l.attempts)
	if index == l.capacity {
		index = l.attemptHead
		delete(l.attemptIndex, l.attempts[index].ID)
		l.attempts[index] = *a
		l.attemptHead = (index + 1) % l.capacity
		l.dropped++
	} else {
		l.attempts = append(l.attempts, *a)
	}
	l.attemptIndex[a.ID] = index
	return a.ID
}
func (l *Ledger) finish(id uint64, u *Usage, known bool, status int, outcome string, price *Price) {
	l.mu.Lock()
	defer l.mu.Unlock()
	index, exists := l.attemptIndex[id]
	if !exists {
		return
	}
	a := &l.attempts[index]
	if !a.FinishedAt.IsZero() {
		return
	}
	a.FinishedAt = time.Now()
	a.Status = status
	a.Outcome = outcome
	a.UsageKnown = known
	a.UsageComplete = known && (outcome == "success" || outcome == "http_error" || outcome == "decode_error")
	if u != nil {
		a.Usage = *u
		if known {
			a.Usage.InputKnown = true
			a.Usage.OutputKnown = true
			a.CostUSD, a.CostKnown = usageCost(u, price)
		}
	}
}
func (l *Ledger) beginOperation(o *OperationRecord) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sequence++
	o.ID = l.sequence
	index := len(l.operations)
	if index == l.capacity {
		index = l.operationHead
		delete(l.operationIndex, l.operations[index].ID)
		l.operations[index] = *o
		l.operationHead = (index + 1) % l.capacity
		l.dropped++
	} else {
		l.operations = append(l.operations, *o)
	}
	l.operationIndex[o.ID] = index
	return o.ID
}
func (l *Ledger) finishOperation(id uint64, outcome string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	index, exists := l.operationIndex[id]
	if !exists {
		return
	}
	o := &l.operations[index]
	if o.FinishedAt.IsZero() {
		o.FinishedAt = time.Now()
		o.Outcome = outcome
	}
}
func (l *Ledger) Snapshot() Snapshot                   { return l.SnapshotFor("", "") }
func (l *Ledger) SnapshotForRun(runID string) Snapshot { return l.SnapshotFor(runID, "") }

// SnapshotFor filters retained records by run and scenario at one observation
// cutoff. Empty filter fields match all. Retention loss remains ledger-wide.
func (l *Ledger) SnapshotFor(runID, scenario string) Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := Snapshot{Version: 1, ObservedAt: time.Now(), Dropped: l.dropped, Incomplete: l.dropped > 0}
	matches := func(a Attribution) bool {
		return (runID == "" || a.RunID == runID) && (scenario == "" || a.Scenario == scenario)
	}
	for offset := range len(l.attempts) {
		a := &l.attempts[(l.attemptHead+offset)%len(l.attempts)]
		if !matches(a.Attribution) {
			continue
		}
		s.Attempts = append(s.Attempts, *a)
		if a.FinishedAt.IsZero() || (a.UsageKnown && !a.UsageComplete) {
			s.Incomplete = true
		}
		if !a.UsageKnown || !a.UsageComplete {
			s.UnknownUsage++
		}
		if !a.CostKnown || !a.UsageComplete {
			s.UnknownCost++
		}
		if a.CostKnown {
			next := s.KnownCostUSD + a.CostUSD
			if math.IsInf(next, 0) || math.IsNaN(next) {
				s.CostOverflow = true
				s.Incomplete = true
			} else {
				s.KnownCostUSD = next
			}
		}
	}
	for offset := range len(l.operations) {
		o := &l.operations[(l.operationHead+offset)%len(l.operations)]
		if !matches(o.Attribution) {
			continue
		}
		s.Operations = append(s.Operations, *o)
		if o.FinishedAt.IsZero() {
			s.Incomplete = true
		}
	}
	return s
}
func usageCost(u *Usage, p *Price) (float64, bool) {
	if u == nil || p == nil {
		return 0, false
	}
	tokens := []int64{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens}
	rates := []*float64{p.Input, p.Output, p.CacheRead, p.CacheWrite}
	cost := 0.0
	for i, n := range tokens {
		if n < 0 {
			return 0, false
		}
		if n == 0 {
			continue
		}
		if rates[i] == nil {
			return 0, false
		}
		cost += float64(n) * ((*rates[i]) / 1e6)
	}
	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0, false
	}
	return cost, true
}
