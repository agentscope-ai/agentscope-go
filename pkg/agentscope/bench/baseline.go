package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Battery support and baseline regression detection (HARNESS_DESIGN G).
// A battery is the standard task set; a baseline pins expected performance
// so CI can flag regressions instead of eyeballing reports.

// NamedScenario pairs a scenario with a stable identity for baselining.
type NamedScenario struct {
	Name     string
	Scenario *Scenario
}

// Battery is the standard benchmark set, run as a unit.
type Battery []NamedScenario

// RunBattery executes every scenario sequentially and returns reports keyed
// by scenario name.
func RunBattery(ctx context.Context, b Battery) (map[string]*Report, error) {
	out := map[string]*Report{}
	for _, ns := range b {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		rep, err := NewRunner().Run(ctx, ns.Scenario)
		if err != nil {
			return out, fmt.Errorf("battery %s: %w", ns.Name, err)
		}
		out[ns.Name] = rep
	}
	return out, nil
}

// BaselineStats pins the expected performance of one scenario.
type BaselineStats struct {
	P95Ms          float64 `json:"p95_ms"`
	MinSuccessRate float64 `json:"min_success_rate"` // 0..1
	MinThroughput  float64 `json:"min_throughput,omitempty"`
}

// Baseline is the persisted performance contract for a battery.
type Baseline struct {
	CreatedAt time.Time                `json:"created_at"`
	Scenarios map[string]BaselineStats `json:"scenarios"`
}

// SaveBaseline writes the baseline JSON to path (atomic).
func SaveBaseline(path string, b *Baseline) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadBaseline reads a baseline JSON file.
func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// BaselineFromReports builds a baseline from observed reports (the
// "record a new baseline" path).
func BaselineFromReports(reports map[string]*Report) *Baseline {
	b := &Baseline{CreatedAt: time.Now(), Scenarios: map[string]BaselineStats{}}
	for name, rep := range reports {
		if rep == nil || rep.Latencies == nil {
			continue
		}
		var successRate float64
		if rep.Iterations > 0 {
			successRate = float64(rep.Successes) / float64(rep.Iterations)
		}
		b.Scenarios[name] = BaselineStats{
			P95Ms:          rep.Latencies.P95.Seconds() * 1000, // fractional ms: sub-ms latencies must not floor to 0 (HARNESS review M10)
			MinSuccessRate: successRate,
			MinThroughput:  rep.Throughput,
		}
	}
	return b
}

// CheckBaseline compares reports against a baseline and returns human-readable
// violations (empty = no regression).
func CheckBaseline(reports map[string]*Report, b *Baseline) []string {
	var violations []string
	for name, stats := range b.Scenarios {
		rep, ok := reports[name]
		if !ok {
			violations = append(violations, fmt.Sprintf("%s: missing from run", name))
			continue
		}
		if rep.Latencies != nil {
			p95 := rep.Latencies.P95.Seconds() * 1000
			if stats.P95Ms > 0 && p95 > stats.P95Ms*1.1 { // 10% slack
				violations = append(violations, fmt.Sprintf(
					"%s: p95 latency regression: %.1fms > baseline %.1fms (+10%% slack)",
					name, p95, stats.P95Ms))
			}
		}
		if rep.Iterations > 0 {
			rate := float64(rep.Successes) / float64(rep.Iterations)
			if stats.MinSuccessRate > 0 && rate < stats.MinSuccessRate {
				violations = append(violations, fmt.Sprintf(
					"%s: success rate regression: %.2f < baseline %.2f",
					name, rate, stats.MinSuccessRate))
			}
		}
	}
	return violations
}
