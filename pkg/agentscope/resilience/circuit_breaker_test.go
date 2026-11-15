package resilience

import (
	"errors"
	"testing"
	"time"
)

func TestCircuitBreakerTripsAfterThreshold(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Minute)
	failing := func() error { return errors.New("boom") }

	for i := 0; i < 3; i++ {
		if got := cb.Execute(failing); got == nil {
			t.Fatalf("call %d: expected error", i)
		}
	}
	if cb.State() != StateOpen {
		t.Fatalf("expected open after threshold, got %s", cb.State())
	}
	if err := cb.Execute(func() error { return nil }); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
}

func TestCircuitBreakerSuccessResetsFailures(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Minute)
	_ = cb.Execute(func() error { return errors.New("boom") })
	_ = cb.Execute(func() error { return errors.New("boom") })
	if err := cb.Execute(func() error { return nil }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cb.State() != StateClosed {
		t.Fatalf("expected closed, got %s", cb.State())
	}
	// Two more failures should not trip (counter was reset).
	_ = cb.Execute(func() error { return errors.New("boom") })
	_ = cb.Execute(func() error { return errors.New("boom") })
	if cb.State() != StateClosed {
		t.Fatalf("expected still closed, got %s", cb.State())
	}
}

func TestCircuitBreakerHalfOpenRecovers(t *testing.T) {
	cb := NewCircuitBreaker(1, 20*time.Millisecond)
	_ = cb.Execute(func() error { return errors.New("boom") })
	if cb.State() != StateOpen {
		t.Fatalf("expected open, got %s", cb.State())
	}

	time.Sleep(30 * time.Millisecond)
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected half-open after timeout, got %s", cb.State())
	}

	if err := cb.Execute(func() error { return nil }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cb.State() != StateClosed {
		t.Fatalf("expected closed after successful trial, got %s", cb.State())
	}
}

func TestCircuitBreakerHalfOpenReopensOnFailure(t *testing.T) {
	cb := NewCircuitBreaker(1, 20*time.Millisecond)
	_ = cb.Execute(func() error { return errors.New("boom") })
	time.Sleep(30 * time.Millisecond)

	// Trial call fails → back to open.
	if err := cb.Execute(func() error { return errors.New("still bad") }); err == nil {
		t.Fatal("expected error from trial call")
	}
	if cb.State() != StateOpen {
		t.Fatalf("expected open after failed trial, got %s", cb.State())
	}
}

func TestCircuitBreakerReset(t *testing.T) {
	cb := NewCircuitBreaker(1, time.Minute)
	_ = cb.Execute(func() error { return errors.New("boom") })
	if cb.State() != StateOpen {
		t.Fatalf("expected open, got %s", cb.State())
	}
	cb.Reset()
	if cb.State() != StateClosed {
		t.Fatalf("expected closed after reset, got %s", cb.State())
	}
	called := false
	_ = cb.Execute(func() error { called = true; return nil })
	if !called {
		t.Fatal("expected call to pass through after reset")
	}
}
