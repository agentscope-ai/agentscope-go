package inference

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestLedgerBoundedOwnedSnapshots(t *testing.T) {
	l := NewLedger(1)
	first := l.begin(&Attempt{OperationID: 1, StartedAt: time.Now()})
	before := l.Snapshot()
	l.finish(first, &Usage{InputTokens: 4}, true, 200, "success", nil)
	if before.Attempts[0].UsageKnown || !before.Incomplete {
		t.Fatal("snapshot mutated by late completion")
	}
	l.begin(&Attempt{OperationID: 2, StartedAt: time.Now()})
	after := l.Snapshot()
	if len(after.Attempts) != 1 || after.Dropped != 1 || !after.Incomplete {
		t.Fatalf("unbounded or hidden loss: %+v", after)
	}
	after.Attempts[0].Purpose = "mutated"
	if l.Snapshot().Attempts[0].Purpose != "" {
		t.Fatal("snapshot aliases ledger")
	}
}
func TestCostUnknownAndCachePricing(t *testing.T) {
	one, two, half := 1.0, 2.0, 0.5
	p := &Price{Input: &one, Output: &two, CacheRead: &half}
	u := &Usage{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 100}
	cost, known := usageCost(u, p)
	if !known || math.Abs(cost-0.00017) > 1e-15 {
		t.Fatalf("cache cost: %v %v", cost, known)
	}
	u.CacheWriteTokens = 1
	if _, known := usageCost(u, p); known {
		t.Fatal("missing cache write price became zero")
	}
}

func TestCostOverflowRemainsSerializable(t *testing.T) {
	rate := math.MaxFloat64
	l := NewLedger(2)
	id := l.begin(&Attempt{})
	l.finish(id, &Usage{InputTokens: math.MaxInt64}, true, 200, "success", &Price{Input: &rate})
	s := l.Snapshot()
	if _, err := json.Marshal(s); err != nil {
		t.Fatalf("snapshot contains nonfinite cost: %v", err)
	}
	if s.Attempts[0].CostKnown {
		t.Fatal("overflow reported known cost")
	}
}

func TestLedgerPartialUsageAggregateOverflowAndRunFilter(t *testing.T) {
	l := NewLedger(8)
	rate := 1e308
	for i := 0; i < 2; i++ {
		id := l.begin(&Attempt{Attribution: Attribution{RunID: "run", Scenario: "s"}})
		l.finish(id, &Usage{InputTokens: 1000000}, true, 200, "success", &Price{Input: &rate})
	}
	id := l.begin(&Attempt{Attribution: Attribution{RunID: "other", Scenario: "s"}})
	l.finish(id, &Usage{InputTokens: 5, InputKnown: true}, false, 200, "canceled", nil)
	filtered := l.SnapshotFor("run", "s")
	if len(filtered.Attempts) != 2 || !filtered.CostOverflow || !filtered.Incomplete {
		t.Fatalf("overflow/filter not explicit: %+v", filtered)
	}
	if _, err := json.Marshal(filtered); err != nil {
		t.Fatal(err)
	}
	other := l.SnapshotForRun("other")
	if other.Attempts[0].Usage.InputTokens != 5 || !other.Attempts[0].Usage.InputKnown || other.Attempts[0].UsageKnown || other.UnknownUsage != 1 {
		t.Fatal("partial usage lost or invented")
	}
}
