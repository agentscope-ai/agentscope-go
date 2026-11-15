package bench

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Scenario defines a load test scenario.
type Scenario struct {
	Name           string
	Concurrency    int           // number of concurrent "users"
	Duration       time.Duration // how long to run (0 = until all iterations done)
	Iterations     int           // total iterations (0 = unlimited, use Duration)
	RampUpDuration time.Duration // time to reach full concurrency
	Setup          func(ctx context.Context) error
	Teardown       func(ctx context.Context) error
	Run            func(ctx context.Context, iteration int) error // the actual workload
}

// Report holds the results of a benchmark run.
type Report struct {
	Scenario   string
	StartTime  time.Time
	EndTime    time.Time
	Duration   time.Duration
	Iterations int64
	Successes  int64
	Failures   int64
	Latencies  *LatencyStats
	Throughput float64          // iterations per second
	Errors     map[string]int64 // error message -> count
}

// LatencyStats holds latency distribution data.
type LatencyStats struct {
	Min  time.Duration
	Max  time.Duration
	Mean time.Duration
	P50  time.Duration
	P95  time.Duration
	P99  time.Duration
}

// Runner executes benchmark scenarios.
type Runner struct{}

// NewRunner creates a new benchmark runner.
func NewRunner() *Runner {
	return &Runner{}
}

// Run executes a scenario and returns the report.
func (r *Runner) Run(ctx context.Context, scenario *Scenario) (*Report, error) {
	if scenario.Run == nil {
		return nil, fmt.Errorf("bench: scenario.Run must not be nil")
	}
	if scenario.Concurrency <= 0 {
		scenario.Concurrency = 1
	}
	if scenario.Duration == 0 && scenario.Iterations == 0 {
		return nil, fmt.Errorf("bench: either Duration or Iterations must be set")
	}

	// Setup
	if scenario.Setup != nil {
		if err := scenario.Setup(ctx); err != nil {
			return nil, fmt.Errorf("bench: setup: %w", err)
		}
	}

	// Prepare context with timeout if duration-based
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if scenario.Duration > 0 {
		runCtx, cancel = context.WithTimeout(ctx, scenario.Duration)
		defer cancel()
	}

	var (
		totalIter atomic.Int64
		successes atomic.Int64
		failures  atomic.Int64
		errMu     sync.Mutex
		errMap    = make(map[string]int64)
		latencyMu sync.Mutex
		latencies []time.Duration
		wg        sync.WaitGroup
		iterCount atomic.Int64 // for iteration-based stopping
	)

	startTime := time.Now()

	// Launch workers with optional ramp-up
	for i := 0; i < scenario.Concurrency; i++ {
		wg.Add(1)
		workerIdx := i

		// Ramp-up delay
		var delay time.Duration
		if scenario.RampUpDuration > 0 && scenario.Concurrency > 1 {
			delay = scenario.RampUpDuration * time.Duration(workerIdx) / time.Duration(scenario.Concurrency-1)
		}

		go func() {
			defer wg.Done()

			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-runCtx.Done():
					return
				}
			}

			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}

				// Check iteration limit
				if scenario.Iterations > 0 {
					iter := iterCount.Add(1)
					if iter > int64(scenario.Iterations) {
						return
					}
				}

				iterNum := int(totalIter.Add(1))
				start := time.Now()
				err := scenario.Run(runCtx, iterNum)
				elapsed := time.Since(start)

				latencyMu.Lock()
				latencies = append(latencies, elapsed)
				latencyMu.Unlock()

				if err != nil {
					failures.Add(1)
					errMu.Lock()
					errMap[err.Error()]++
					errMu.Unlock()
				} else {
					successes.Add(1)
				}
			}
		}()
	}

	wg.Wait()
	endTime := time.Now()

	// Teardown
	if scenario.Teardown != nil {
		if err := scenario.Teardown(ctx); err != nil {
			return nil, fmt.Errorf("bench: teardown: %w", err)
		}
	}

	duration := endTime.Sub(startTime)
	totalIterations := totalIter.Load()

	report := &Report{
		Scenario:   scenario.Name,
		StartTime:  startTime,
		EndTime:    endTime,
		Duration:   duration,
		Iterations: totalIterations,
		Successes:  successes.Load(),
		Failures:   failures.Load(),
		Latencies:  computeLatencyStats(latencies),
		Throughput: float64(totalIterations) / duration.Seconds(),
		Errors:     errMap,
	}

	return report, nil
}

func computeLatencyStats(latencies []time.Duration) *LatencyStats {
	if len(latencies) == 0 {
		return &LatencyStats{}
	}

	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	var total time.Duration
	for _, l := range latencies {
		total += l
	}

	n := len(latencies)
	return &LatencyStats{
		Min:  latencies[0],
		Max:  latencies[n-1],
		Mean: total / time.Duration(n),
		P50:  percentile(latencies, 0.50),
		P95:  percentile(latencies, 0.95),
		P99:  percentile(latencies, 0.99),
	}
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
