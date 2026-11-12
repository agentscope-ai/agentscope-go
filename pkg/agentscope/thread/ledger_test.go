package thread

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// flatPricer charges one cent per thousand input or output tokens and
// cannot price the "free" participant's classifier calls.
func flatPricer(participant string, kind UsageKind, u *model.ChatUsage) (float64, bool) {
	if participant == "free" && kind == UsageClassifier {
		return 0, false
	}
	return float64(u.InputTokens+u.OutputTokens) / 1000 * 0.01, true
}

func usage(in, out, cacheRead, cacheWrite int) *model.ChatUsage {
	return &model.ChatUsage{InputTokens: in, OutputTokens: out, CacheInputTokens: cacheRead, CacheCreationInputTokens: cacheWrite}
}

func costOf(t LedgerTotals) float64 {
	if t.CostUSD == nil {
		return math.NaN()
	}
	return *t.CostUSD
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestLedgerTotals(t *testing.T) {
	l := NewLedger(flatPricer)
	l.Record(LedgerEntry{Participant: "cloud", Path: "master", Kind: UsageMain, Usage: usage(1000, 1000, 500, 100)})
	l.Record(LedgerEntry{Participant: "cloud", Path: "fork", Kind: UsageForkPeer, Usage: usage(2000, 0, 0, 0)})
	l.Record(LedgerEntry{Participant: "edge", Path: "master", Kind: UsageMain, Usage: usage(3000, 1000, 0, 0)})
	l.Record(LedgerEntry{Participant: "edge", Path: "master", Kind: UsageCompaction}) // unknown usage

	totals := l.Totals()
	cloud := totals["cloud"]
	if cloud.Calls != 2 || cloud.UnknownCalls != 0 || cloud.InputTokens != 3000 || cloud.OutputTokens != 1000 ||
		cloud.CacheReadTokens != 500 || cloud.CacheWriteTokens != 100 || !near(costOf(cloud), 0.04) {
		t.Errorf("cloud totals %+v cost %v", cloud, costOf(cloud))
	}
	edge := totals["edge"]
	if edge.Calls != 2 || edge.UnknownCalls != 1 || edge.InputTokens != 3000 || edge.CostUSD != nil {
		t.Errorf("edge totals %+v: an unknown call must leave the cost unknown", edge)
	}
	all := totals[""]
	if all.Calls != 4 || all.UnknownCalls != 1 || all.InputTokens != 6000 || all.OutputTokens != 2000 || all.CostUSD != nil {
		t.Errorf("overall totals %+v", all)
	}

	byKind := l.TotalsByKind()
	if main := byKind[UsageMain]; main.Calls != 2 || !near(costOf(main), 0.06) {
		t.Errorf("main totals %+v cost %v", main, costOf(main))
	}
	if peer := byKind[UsageForkPeer]; peer.Calls != 1 || !near(costOf(peer), 0.02) {
		t.Errorf("fork_peer totals %+v", peer)
	}
	if comp := byKind[UsageCompaction]; comp.Calls != 1 || comp.UnknownCalls != 1 || comp.CostUSD != nil {
		t.Errorf("compaction totals %+v", comp)
	}
	if _, ok := byKind[UsageForkOwn]; ok {
		t.Error("a kind with no calls has totals")
	}
}

func TestLedgerUnpriceable(t *testing.T) {
	l := NewLedger(flatPricer)
	l.Record(LedgerEntry{Participant: "free", Kind: UsageClassifier, Usage: usage(10, 1, 0, 0)})
	l.Record(LedgerEntry{Participant: "free", Kind: UsageMain, Usage: usage(1000, 0, 0, 0)})
	totals := l.Totals()
	if totals["free"].CostUSD != nil || totals[""].CostUSD != nil {
		t.Errorf("an unpriceable call left a cost: %+v", totals)
	}
	if main := l.TotalsByKind()[UsageMain]; !near(costOf(main), 0.01) {
		t.Errorf("priced kind lost its cost: %+v", main)
	}

	bad := []float64{math.NaN(), math.Inf(1), -1}
	for _, v := range bad {
		l := NewLedger(func(string, UsageKind, *model.ChatUsage) (float64, bool) { return v, true })
		l.Record(LedgerEntry{Participant: "p", Kind: UsageMain, Usage: usage(1, 1, 0, 0)})
		if c := l.Totals()["p"].CostUSD; c != nil {
			t.Errorf("pricer result %v accepted as cost %v", v, *c)
		}
	}

	none := NewLedger(nil)
	none.Record(LedgerEntry{Participant: "p", Kind: UsageMain, Usage: usage(1, 1, 0, 0)})
	if c := none.Totals()["p"]; c.CostUSD != nil || c.InputTokens != 1 {
		t.Errorf("a nil pricer gave %+v", c)
	}
}

func TestLedgerEmptyAndAnonymous(t *testing.T) {
	l := NewLedger(nil)
	totals := l.Totals()
	if len(totals) != 1 || totals[""].Calls != 0 || costOf(totals[""]) != 0 {
		t.Fatalf("empty ledger totals %+v", totals)
	}
	if len(l.TotalsByKind()) != 0 || len(l.Entries()) != 0 {
		t.Fatal("empty ledger has entries")
	}
	l.Record(LedgerEntry{Kind: UsageClassifier})
	totals = l.Totals()
	if len(totals) != 1 || totals[""].Calls != 1 || totals[""].UnknownCalls != 1 {
		t.Errorf("an anonymous call: %+v", totals)
	}
}

func TestLedgerEntriesAreCopies(t *testing.T) {
	l := NewLedger(nil)
	u := usage(5, 5, 0, 0)
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.Record(LedgerEntry{Participant: "p", Kind: UsageMain, Usage: u, Time: stamp})
	l.Record(LedgerEntry{Participant: "p", Kind: UsageForkOwn})
	u.InputTokens = 999
	got := l.Entries()
	got[0].Usage.InputTokens = 777
	got[0].Participant = "changed"
	again := l.Entries()
	if again[0].Usage.InputTokens != 5 || again[0].Participant != "p" || !again[0].Time.Equal(stamp) {
		t.Errorf("ledger entry changed through a copy: %+v %+v", again[0], again[0].Usage)
	}
	if again[1].Usage != nil || again[1].Time.IsZero() {
		t.Errorf("unknown usage entry %+v", again[1])
	}
}

func TestLedgerConcurrent(t *testing.T) {
	l := NewLedger(flatPricer)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				l.Record(LedgerEntry{Participant: "p", Kind: UsageMain, Usage: usage(1, 0, 0, 0)})
				_ = l.Totals()
			}
		}()
	}
	wg.Wait()
	if got := l.Totals()["p"]; got.Calls != 800 || got.InputTokens != 800 {
		t.Errorf("concurrent totals %+v", got)
	}
}
