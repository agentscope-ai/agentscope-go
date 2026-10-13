package managedtask

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAdmissionFIFOAndCanceledHandoff(t *testing.T) {
	for i := 0; i < 30; i++ {
		a := &admission{maxActive: 1, maxQueued: 2}
		if err := a.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
		firstCtx, cancel := context.WithCancel(context.Background())
		first := make(chan error, 1)
		second := make(chan error, 1)
		go func() { first <- a.acquire(firstCtx) }()
		waitQueue := func(n int) {
			t.Helper()
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				a.mu.Lock()
				q := len(a.queue)
				a.mu.Unlock()
				if q == n {
					return
				}
				time.Sleep(time.Millisecond)
			}
			t.Fatal("waiter not queued")
		}
		waitQueue(1)
		go func() { second <- a.acquire(context.Background()) }()
		waitQueue(2)
		if err := a.acquire(context.Background()); !errors.Is(err, ErrCapacity) {
			t.Fatal("queue cap bypassed")
		}
		a.release()
		err := <-first
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-second:
			t.Fatal("FIFO order bypassed")
		default:
		}
		a.release()
		if err = <-second; err != nil {
			t.Fatal(err)
		}
		a.release()
		cancel()
		// Race cancellation against granting the head. The canceled acquirer owns
		// the handoff cleanup if it was granted; otherwise the queue removes it.
		if err = a.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, stop := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.acquire(ctx) }()
		waitQueue(1)
		stop()
		a.release()
		err = <-done
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation ignored")
		}
		a.mu.Lock()
		active, queued := a.active, len(a.queue)
		a.mu.Unlock()
		if active != 0 || queued != 0 {
			t.Fatalf("permit leaked: active=%d queue=%d", active, queued)
		}
	}
}
