package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/runtime"
)

// This example demonstrates the handler-based Pool for fan-out agent workloads.
// It shows how to:
// 1. Create a Pool with 5 workers and a queue of 20
// 2. Define a handler that simulates variable-latency work
// 3. Submit 15 requests concurrently
// 4. Collect results via channels
// 5. Print pool statistics (completed, failed, active)
// 6. Shut down gracefully

func main() {
	fmt.Println("=== Fan-out Agent Pool Example ===")
	fmt.Println()

	// Step 1: Configure and create the pool.
	cfg := runtime.PoolConfig{
		MaxWorkers:    5,
		QueueSize:     20,
		WorkerTimeout: 10 * time.Second,
	}

	// Step 2: Define a handler that simulates work.
	// Each request sleeps for a random duration (5-50ms) and occasionally fails.
	handler := func(ctx context.Context, req *runtime.Request) *runtime.Result {
		// Simulate variable-latency processing.
		delay := time.Duration(5+rand.Intn(45)) * time.Millisecond
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return &runtime.Result{
				RequestID: req.ID,
				Error:     ctx.Err(),
			}
		}

		// Simulate 10% failure rate.
		if rand.Intn(10) == 0 {
			return &runtime.Result{
				RequestID: req.ID,
				Error:     fmt.Errorf("simulated failure for request %s", req.ID),
			}
		}

		return &runtime.Result{
			RequestID: req.ID,
			Output:    fmt.Sprintf("Processed: %s -> result OK", req.Input),
		}
	}

	pool := runtime.NewPool(cfg, handler)
	fmt.Printf("Pool started: workers=%d, queue_size=%d\n", cfg.MaxWorkers, cfg.QueueSize)
	fmt.Println()

	// Step 3: Submit 15 requests concurrently.
	const numRequests = 15
	var wg sync.WaitGroup
	results := make([]*runtime.Result, numRequests)
	var mu sync.Mutex

	fmt.Printf("Submitting %d requests...\n", numRequests)
	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			// Each request gets its own result channel.
			resultCh := make(chan *runtime.Result, 1)
			req := &runtime.Request{
				ID:       fmt.Sprintf("req-%03d", idx),
				Input:    fmt.Sprintf("task-%d", idx),
				Ctx:      context.Background(),
				ResultCh: resultCh,
			}

			if err := pool.Submit(req); err != nil {
				mu.Lock()
				results[idx] = &runtime.Result{
					RequestID: req.ID,
					Error:     fmt.Errorf("submit failed: %w", err),
				}
				mu.Unlock()
				return
			}

			// Wait for the result.
			res := <-resultCh
			mu.Lock()
			results[idx] = res
			mu.Unlock()
		}(i)
	}

	// Step 4: Wait for all results.
	wg.Wait()
	fmt.Println("All requests completed.")
	fmt.Println()

	// Step 5: Print results and statistics.
	var succeeded, failed int
	for _, r := range results {
		if r == nil {
			continue
		}
		if r.Error != nil {
			failed++
			fmt.Printf("  FAIL [%s]: %v (duration=%v)\n", r.RequestID, r.Error, r.Duration)
		} else {
			succeeded++
			fmt.Printf("  OK   [%s]: %s (duration=%v)\n", r.RequestID, r.Output, r.Duration)
		}
	}

	fmt.Println()
	fmt.Printf("--- Results Summary ---\n")
	fmt.Printf("  Succeeded: %d\n", succeeded)
	fmt.Printf("  Failed:    %d\n", failed)
	fmt.Printf("  Total:     %d\n", succeeded+failed)

	// Pool stats from the internal counters.
	stats := pool.Stats()
	fmt.Println()
	fmt.Printf("--- Pool Stats ---\n")
	fmt.Printf("  Active workers:  %d\n", stats.ActiveWorkers)
	fmt.Printf("  Pending jobs:    %d\n", stats.PendingJobs)
	fmt.Printf("  Completed jobs:  %d\n", stats.CompletedJobs)
	fmt.Printf("  Failed jobs:     %d\n", stats.FailedJobs)
	fmt.Printf("  Total duration:  %v\n", stats.TotalDuration)
	fmt.Println()

	// Step 6: Graceful shutdown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := pool.Shutdown(shutdownCtx); err != nil {
		fmt.Println("Shutdown error:", err)
	} else {
		fmt.Println("Pool shut down gracefully.")
	}
}
