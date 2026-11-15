package resilience

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestCircuitBreaker_HalfOpenSingleProbe proves that while half-open the breaker
// admits exactly one trial call; concurrent callers are rejected with
// ErrCircuitOpen (previously half-open admitted unbounded concurrent probes,
// defeating the point of protecting a recovering dependency).
func TestCircuitBreaker_HalfOpenSingleProbe(t *testing.T) {
	cb := NewCircuitBreaker(1, 10*time.Millisecond)

	// Trip the breaker open.
	_ = cb.Execute(func() error { return errors.New("boom") })

	// Wait past the reset timeout so the next call becomes the half-open probe.
	time.Sleep(20 * time.Millisecond)

	var admitted int32
	started := make(chan struct{})
	release := make(chan struct{})

	go func() {
		_ = cb.Execute(func() error {
			atomic.AddInt32(&admitted, 1)
			close(started)
			<-release
			return nil
		})
	}()

	<-started // first probe is now in flight

	// A second concurrent call while half-open must be rejected.
	err := cb.Execute(func() error {
		atomic.AddInt32(&admitted, 1)
		return nil
	})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen for concurrent half-open probe, got %v", err)
	}

	close(release)
	if got := atomic.LoadInt32(&admitted); got != 1 {
		t.Fatalf("expected exactly 1 admitted probe, got %d", got)
	}
}
