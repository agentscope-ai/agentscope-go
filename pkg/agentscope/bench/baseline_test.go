package bench

import (
	"path/filepath"
	"testing"
	"time"
)

func TestBaseline_SaveLoadCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")

	reports := map[string]*Report{
		"loop-basic": {
			Scenario:   "loop-basic",
			Iterations: 100,
			Successes:  100,
			Throughput: 50,
			Latencies:  &LatencyStats{P95: 20 * time.Millisecond},
		},
	}
	bl := BaselineFromReports(reports)
	if err := SaveBaseline(path, bl); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Scenarios) != 1 || loaded.Scenarios["loop-basic"].P95Ms != 20 {
		t.Fatalf("round-trip failed: %+v", loaded.Scenarios)
	}

	// Same performance → no violations.
	if v := CheckBaseline(reports, loaded); len(v) != 0 {
		t.Errorf("unexpected violations: %v", v)
	}

	// P95 regresses beyond slack → violation.
	regressed := map[string]*Report{
		"loop-basic": {
			Iterations: 100,
			Successes:  100,
			Latencies:  &LatencyStats{P95: 30 * time.Millisecond},
		},
	}
	v := CheckBaseline(regressed, loaded)
	if len(v) != 1 {
		t.Fatalf("expected 1 violation, got %v", v)
	}

	// Success rate regresses → violation.
	failures := map[string]*Report{
		"loop-basic": {Iterations: 100, Successes: 80, Latencies: &LatencyStats{P95: 10 * time.Millisecond}},
	}
	v = CheckBaseline(failures, loaded)
	if len(v) != 1 {
		t.Fatalf("expected success-rate violation, got %v", v)
	}

	// Missing scenario → violation.
	if v := CheckBaseline(map[string]*Report{}, loaded); len(v) != 1 {
		t.Fatalf("expected missing-scenario violation, got %v", v)
	}
}
