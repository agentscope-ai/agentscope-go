package thread

import (
	"math"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// UsageKind says what a model call was for.
type UsageKind string

const (
	// UsageMain is a participant's ordinary agent call.
	UsageMain UsageKind = "main"
	// UsageForkPeer is a summary fork written for another participant's
	// window.
	UsageForkPeer UsageKind = "fork_peer"
	// UsageForkOwn is a summary fork written for the participant's own
	// window.
	UsageForkOwn UsageKind = "fork_own"
	// UsageClassifier is a routing classifier call.
	UsageClassifier UsageKind = "classifier"
	// UsageCompaction is a context compaction call.
	UsageCompaction UsageKind = "compaction"
)

// LedgerEntry records one model call.
type LedgerEntry struct {
	Participant string
	// Path is the agent path inside the participant, such as master.
	Path string
	Kind UsageKind
	// Usage is what the provider reported; nil means the call happened but
	// its usage is unknown.
	Usage *model.ChatUsage
	Time  time.Time
}

// Pricer returns the cost of usage for a participant, and false when it
// cannot price it.
type Pricer func(participant string, kind UsageKind, u *model.ChatUsage) (usd float64, ok bool)

// Ledger is the shared accounting of model calls across participants and
// agent paths. Unknown usage stays unknown: it is counted, never guessed. It
// is safe for concurrent use.
type Ledger struct {
	mu      sync.Mutex
	pricer  Pricer
	entries []LedgerEntry
}

// NewLedger returns an empty ledger. A nil pricer leaves every cost unknown.
func NewLedger(p Pricer) *Ledger {
	return &Ledger{pricer: p}
}

// Record adds one call. A zero Time is set to the current time. The usage
// is copied.
func (l *Ledger) Record(e LedgerEntry) { //nolint:gocritic // value parameter keeps the documented API
	e.Usage = cloneUsage(e.Usage)
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	l.mu.Lock()
	l.entries = append(l.entries, e)
	l.mu.Unlock()
}

// Entries returns a copy of every recorded call, oldest first.
func (l *Ledger) Entries() []LedgerEntry {
	l.mu.Lock()
	out := make([]LedgerEntry, len(l.entries))
	copy(out, l.entries)
	l.mu.Unlock()
	for i := range out {
		out[i].Usage = cloneUsage(out[i].Usage)
	}
	return out
}

// LedgerTotals sums recorded calls.
type LedgerTotals struct {
	// Calls counts every call; UnknownCalls those whose usage is unknown.
	Calls, UnknownCalls int
	// Token sums cover the calls whose usage is known. CacheReadTokens sums
	// ChatUsage.CacheInputTokens and CacheWriteTokens sums
	// ChatUsage.CacheCreationInputTokens.
	InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens int
	// CostUSD is nil when any call is unknown or cannot be priced.
	CostUSD *float64
}

// Totals sums the calls by participant, plus key "" for the total, which
// is present even for an empty ledger. Calls recorded with an empty
// participant count only toward the total.
func (l *Ledger) Totals() map[string]LedgerTotals {
	out := groupTotals(l, func(e *LedgerEntry, add func(string)) {
		add("")
		if e.Participant != "" {
			add(e.Participant)
		}
	})
	if _, ok := out[""]; !ok {
		zero := 0.0
		out[""] = LedgerTotals{CostUSD: &zero}
	}
	return out
}

// TotalsByKind sums the calls by usage kind.
func (l *Ledger) TotalsByKind() map[UsageKind]LedgerTotals {
	return groupTotals(l, func(e *LedgerEntry, add func(UsageKind)) { add(e.Kind) })
}

// groupTotals sums the ledger's calls into the groups keys names for each
// call. The pricer runs without the ledger lock held.
func groupTotals[K comparable](l *Ledger, keys func(e *LedgerEntry, add func(K))) map[K]LedgerTotals {
	entries := l.Entries()
	acc := make(map[K]*totalsAcc)
	for i := range entries {
		e := &entries[i]
		usd, priced := l.price(e)
		keys(e, func(k K) {
			a, ok := acc[k]
			if !ok {
				a = &totalsAcc{priced: true}
				acc[k] = a
			}
			a.add(e, usd, priced)
		})
	}
	out := make(map[K]LedgerTotals, len(acc))
	for k, a := range acc {
		out[k] = a.totals()
	}
	return out
}

func (l *Ledger) price(e *LedgerEntry) (float64, bool) {
	if e.Usage == nil || l.pricer == nil {
		return 0, false
	}
	usd, ok := l.pricer(e.Participant, e.Kind, e.Usage)
	if !ok || math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		return 0, false
	}
	return usd, true
}

type totalsAcc struct {
	t      LedgerTotals
	cost   float64
	priced bool
}

func (a *totalsAcc) add(e *LedgerEntry, usd float64, priced bool) {
	a.t.Calls++
	if e.Usage == nil {
		a.t.UnknownCalls++
	} else {
		a.t.InputTokens += e.Usage.InputTokens
		a.t.OutputTokens += e.Usage.OutputTokens
		a.t.CacheReadTokens += e.Usage.CacheInputTokens
		a.t.CacheWriteTokens += e.Usage.CacheCreationInputTokens
	}
	if priced {
		a.cost += usd
	} else {
		a.priced = false
	}
}

func (a *totalsAcc) totals() LedgerTotals {
	t := a.t
	if a.priced {
		c := a.cost
		t.CostUSD = &c
	}
	return t
}

func cloneUsage(u *model.ChatUsage) *model.ChatUsage {
	if u == nil {
		return nil
	}
	c := *u
	return &c
}
