package main

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/bench"
)

// This example demonstrates the agent load testing framework.
// It shows how to:
// 1. Define a Scenario with concurrency and iteration settings
// 2. Simulate a variable-latency workload with random failures
// 3. Run the benchmark
// 4. Print the Report with throughput, latency percentiles, and error counts

func main() {
	fmt.Println("=== Agent Load Testing (Bench) Example ===")
	fmt.Println()

	// Step 1: Define the benchmark scenario.
	scenario := &bench.Scenario{
		Name:        "simulated-agent-workload",
		Concurrency: 10,  // 10 concurrent workers
		Iterations:  100, // 100 total iterations
		// Run simulates a variable-latency task (1-10ms) with 10% failure rate.
		Run: func(ctx context.Context, iteration int) error {
			// Simulate work with random latency between 1ms and 10ms.
			delay := time.Duration(1+rand.Intn(10)) * time.Millisecond
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}

			// Simulate 10% failure rate.
			if rand.Intn(10) == 0 {
				return fmt.Errorf("agent timeout on iteration %d", iteration)
			}
			return nil
		},
	}

	fmt.Printf("Scenario: %s\n", scenario.Name)
	fmt.Printf("  Concurrency: %d workers\n", scenario.Concurrency)
	fmt.Printf("  Iterations:  %d\n", scenario.Iterations)
	fmt.Println()

	// Step 2: Run the benchmark.
	fmt.Println("Running benchmark...")
	runner := bench.NewRunner()
	report, err := runner.Run(context.Background(), scenario)
	if err != nil {
		fmt.Println("Benchmark error:", err)
		return
	}
	fmt.Println("Benchmark complete!")
	fmt.Println()

	// Step 3: Print the report.
	fmt.Println("=== Benchmark Report ===")
	fmt.Printf("  Duration:    %v\n", report.Duration)
	fmt.Printf("  Iterations:  %d\n", report.Iterations)
	fmt.Printf("  Successes:   %d\n", report.Successes)
	fmt.Printf("  Failures:    %d\n", report.Failures)
	fmt.Printf("  Throughput:  %.1f iter/sec\n", report.Throughput)
	fmt.Println()

	// Latency distribution.
	fmt.Println("--- Latency Distribution ---")
	if report.Latencies != nil {
		fmt.Printf("  Min:  %v\n", report.Latencies.Min)
		fmt.Printf("  Max:  %v\n", report.Latencies.Max)
		fmt.Printf("  Mean: %v\n", report.Latencies.Mean)
		fmt.Printf("  P50:  %v\n", report.Latencies.P50)
		fmt.Printf("  P95:  %v\n", report.Latencies.P95)
		fmt.Printf("  P99:  %v\n", report.Latencies.P99)
	}
	fmt.Println()

	// Error breakdown (if any).
	if len(report.Errors) > 0 {
		fmt.Println("--- Error Breakdown ---")
		for errMsg, count := range report.Errors {
			fmt.Printf("  [%d] %s\n", count, errMsg)
		}
		fmt.Println()
	}

	// Summary.
	successRate := float64(report.Successes) / float64(report.Iterations) * 100
	fmt.Printf("Success rate: %.1f%%\n", successRate)
}
