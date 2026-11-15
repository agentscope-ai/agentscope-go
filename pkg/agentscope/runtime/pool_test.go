package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPool_BasicSubmitAndReceive(t *testing.T) {
	handler := func(ctx context.Context, req *Request) *Result {
		return &Result{
			RequestID: req.ID,
			Output:    "echo:" + req.Input,
		}
	}

	p := NewPool(PoolConfig{MaxWorkers: 2, QueueSize: 10, WorkerTimeout: time.Second}, handler)
	defer p.Shutdown(context.Background())

	resultCh := make(chan *Result, 1)
	err := p.Submit(&Request{
		ID:       "r1",
		Input:    "hello",
		Ctx:      context.Background(),
		ResultCh: resultCh,
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	select {
	case r := <-resultCh:
		if r.RequestID != "r1" {
			t.Errorf("expected RequestID r1, got %s", r.RequestID)
		}
		if r.Output != "echo:hello" {
			t.Errorf("expected output echo:hello, got %s", r.Output)
		}
		if r.Error != nil {
			t.Errorf("expected no error, got %v", r.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result")
	}
}

func TestPool_MultipleConcurrentRequests(t *testing.T) {
	var processed atomic.Int64
	handler := func(ctx context.Context, req *Request) *Result {
		time.Sleep(10 * time.Millisecond)
		processed.Add(1)
		return &Result{
			RequestID: req.ID,
			Output:    "done:" + req.Input,
		}
	}

	p := NewPool(PoolConfig{MaxWorkers: 4, QueueSize: 50, WorkerTimeout: 5 * time.Second}, handler)
	defer p.Shutdown(context.Background())

	const n = 20
	channels := make([]<-chan *Result, n)
	for i := 0; i < n; i++ {
		ch := make(chan *Result, 1)
		err := p.Submit(&Request{
			ID:       fmt.Sprintf("req-%d", i),
			Input:    "work",
			Ctx:      context.Background(),
			ResultCh: ch,
		})
		if err != nil {
			t.Fatalf("Submit %d failed: %v", i, err)
		}
		channels[i] = ch
	}

	for i, ch := range channels {
		select {
		case r := <-ch:
			if r.Error != nil {
				t.Errorf("request %d failed: %v", i, r.Error)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("request %d timed out", i)
		}
	}

	if got := processed.Load(); got != n {
		t.Errorf("expected %d processed, got %d", n, got)
	}
}

func TestPool_Backpressure(t *testing.T) {
	// Handler that blocks indefinitely until context is canceled.
	handler := func(ctx context.Context, req *Request) *Result {
		<-ctx.Done()
		return &Result{RequestID: req.ID, Error: ctx.Err()}
	}

	// 1 worker, queue of 2 — fill the worker + queue, then next submit should fail.
	p := NewPool(PoolConfig{MaxWorkers: 1, QueueSize: 2, WorkerTimeout: 10 * time.Second}, handler)

	// Fill: 1 goes to the worker (blocks), 2 fill the queue.
	for i := 0; i < 3; i++ {
		ch := make(chan *Result, 1)
		err := p.Submit(&Request{
			ID:       "fill",
			Input:    "x",
			Ctx:      context.Background(),
			ResultCh: ch,
		})
		if err != nil {
			t.Fatalf("submit %d should succeed but got: %v", i, err)
		}
		// Give the first request time to be picked up by the worker.
		if i == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Now the queue should be full — next submit gets ErrPoolFull.
	ch := make(chan *Result, 1)
	err := p.Submit(&Request{ID: "overflow", Input: "y", Ctx: context.Background(), ResultCh: ch})
	if !errors.Is(err, ErrPoolFull) {
		t.Fatalf("expected ErrPoolFull, got %v", err)
	}

	// Cleanup.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = p.Shutdown(ctx)
}

func TestPool_ShutdownWaitsForInFlight(t *testing.T) {
	var completed atomic.Int64
	handler := func(ctx context.Context, req *Request) *Result {
		time.Sleep(50 * time.Millisecond)
		completed.Add(1)
		return &Result{RequestID: req.ID, Output: "done"}
	}

	p := NewPool(PoolConfig{MaxWorkers: 2, QueueSize: 10, WorkerTimeout: 5 * time.Second}, handler)

	for i := 0; i < 4; i++ {
		ch := make(chan *Result, 1)
		_ = p.Submit(&Request{ID: "s", Input: "w", Ctx: context.Background(), ResultCh: ch})
	}

	// Give time for at least one to start.
	time.Sleep(10 * time.Millisecond)

	err := p.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	if got := completed.Load(); got != 4 {
		t.Errorf("expected 4 completed, got %d", got)
	}
}

func TestPool_Stats(t *testing.T) {
	handler := func(ctx context.Context, req *Request) *Result {
		if req.Input == "fail" {
			return &Result{RequestID: req.ID, Error: errors.New("deliberate")}
		}
		return &Result{RequestID: req.ID, Output: "ok"}
	}

	p := NewPool(PoolConfig{MaxWorkers: 2, QueueSize: 10, WorkerTimeout: time.Second}, handler)

	// Submit 3 success + 2 fail.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		input := "ok"
		if i >= 3 {
			input = "fail"
		}
		ch := make(chan *Result, 1)
		_ = p.Submit(&Request{ID: "stat", Input: input, Ctx: context.Background(), ResultCh: ch})
		go func() {
			defer wg.Done()
			<-ch
		}()
	}
	wg.Wait()

	stats := p.Stats()
	if stats.CompletedJobs != 3 {
		t.Errorf("expected 3 completed, got %d", stats.CompletedJobs)
	}
	if stats.FailedJobs != 2 {
		t.Errorf("expected 2 failed, got %d", stats.FailedJobs)
	}
	if stats.ActiveWorkers != 0 {
		t.Errorf("expected 0 active workers after completion, got %d", stats.ActiveWorkers)
	}

	_ = p.Shutdown(context.Background())
}

func TestPool_HandlerTimeout(t *testing.T) {
	handler := func(ctx context.Context, req *Request) *Result {
		// Simulate long work — should be interrupted by WorkerTimeout.
		select {
		case <-ctx.Done():
			return &Result{RequestID: req.ID, Error: ctx.Err()}
		case <-time.After(10 * time.Second):
			return &Result{RequestID: req.ID, Output: "should not reach"}
		}
	}

	p := NewPool(PoolConfig{MaxWorkers: 1, QueueSize: 5, WorkerTimeout: 50 * time.Millisecond}, handler)
	defer p.Shutdown(context.Background())

	ch := make(chan *Result, 1)
	err := p.Submit(&Request{ID: "timeout", Input: "slow", Ctx: context.Background(), ResultCh: ch})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	select {
	case r := <-ch:
		if r.Error == nil {
			t.Fatal("expected timeout error, got nil")
		}
		if !errors.Is(r.Error, context.DeadlineExceeded) {
			t.Fatalf("expected DeadlineExceeded, got %v", r.Error)
		}
		// Duration should be roughly the timeout.
		if r.Duration < 40*time.Millisecond || r.Duration > 200*time.Millisecond {
			t.Errorf("unexpected duration: %v", r.Duration)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result")
	}

	stats := p.Stats()
	if stats.FailedJobs != 1 {
		t.Errorf("expected 1 failed job, got %d", stats.FailedJobs)
	}
}

func TestPool_ShutdownContextDeadlineExceeded(t *testing.T) {
	handler := func(ctx context.Context, req *Request) *Result {
		// Block for a long time.
		<-ctx.Done()
		return &Result{RequestID: req.ID, Error: ctx.Err()}
	}

	p := NewPool(PoolConfig{MaxWorkers: 1, QueueSize: 5, WorkerTimeout: 10 * time.Second}, handler)

	ch := make(chan *Result, 1)
	_ = p.Submit(&Request{ID: "long", Input: "x", Ctx: context.Background(), ResultCh: ch})

	// Give the worker time to start.
	time.Sleep(20 * time.Millisecond)

	// Shutdown with a very short deadline — should return context.DeadlineExceeded.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := p.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded from Shutdown, got %v", err)
	}

	// Workers should still eventually finish (the queue was closed, and
	// WorkerTimeout will fire). Wait for the result to confirm cleanup.
	select {
	case r := <-ch:
		if r == nil {
			t.Fatal("expected a result, got nil")
		}
	case <-time.After(12 * time.Second):
		t.Fatal("worker did not clean up within WorkerTimeout")
	}
}

func TestPool_SubmitAfterShutdown(t *testing.T) {
	handler := func(ctx context.Context, req *Request) *Result {
		return &Result{RequestID: req.ID, Output: "ok"}
	}

	p := NewPool(PoolConfig{MaxWorkers: 1, QueueSize: 5, WorkerTimeout: time.Second}, handler)
	_ = p.Shutdown(context.Background())

	ch := make(chan *Result, 1)
	err := p.Submit(&Request{ID: "late", Input: "x", Ctx: context.Background(), ResultCh: ch})
	if !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("expected ErrPoolClosed, got %v", err)
	}
}
