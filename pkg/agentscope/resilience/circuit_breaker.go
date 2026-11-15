// Package resilience provides fault-tolerance wrappers (circuit breaker, rate
// limiter) for model.ChatModel and other call-based operations.
package resilience

import (
	"errors"
	"sync"
	"time"
)

// CircuitState is the current state of a CircuitBreaker.
type CircuitState int

const (
	// StateClosed is normal operation: calls pass through and failures are counted.
	StateClosed CircuitState = iota
	// StateOpen rejects all calls until resetTimeout elapses.
	StateOpen
	// StateHalfOpen allows a single trial call to test recovery.
	StateHalfOpen
)

// String returns a human-readable name for the state.
func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// ErrCircuitOpen is returned by Execute when the breaker is open and rejecting calls.
var ErrCircuitOpen = errors.New("circuit breaker is open")

// CircuitBreaker trips open after a threshold of consecutive failures, rejects
// calls for resetTimeout, then allows a single trial call to test recovery.
type CircuitBreaker struct {
	threshold    int
	resetTimeout time.Duration

	mu              sync.Mutex
	state           CircuitState
	failures        int
	lastFailure     time.Time
	halfOpenProbing bool // true while a single half-open trial call is in flight
}

// NewCircuitBreaker creates a breaker that trips after threshold consecutive
// failures and stays open for resetTimeout before testing recovery.
func NewCircuitBreaker(threshold int, resetTimeout time.Duration) *CircuitBreaker {
	if threshold < 1 {
		threshold = 1
	}
	return &CircuitBreaker{
		threshold:    threshold,
		resetTimeout: resetTimeout,
		state:        StateClosed,
	}
}

// Execute runs fn if the breaker permits it, updating state based on the result.
// It returns ErrCircuitOpen without calling fn when the breaker is open.
func (cb *CircuitBreaker) Execute(fn func() error) error {
	if err := cb.beforeCall(); err != nil {
		return err
	}
	err := fn()
	cb.afterCall(err)
	return err
}

// beforeCall checks whether a call may proceed and transitions Open→HalfOpen
// once the reset timeout has elapsed.
func (cb *CircuitBreaker) beforeCall() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateOpen:
		if time.Since(cb.lastFailure) >= cb.resetTimeout {
			// Transition to half-open and admit this call as the single probe.
			cb.state = StateHalfOpen
			cb.halfOpenProbing = true
			return nil
		}
		return ErrCircuitOpen
	case StateHalfOpen:
		// Only one probe is allowed at a time while recovering.
		if cb.halfOpenProbing {
			return ErrCircuitOpen
		}
		cb.halfOpenProbing = true
		return nil
	}
	return nil
}

// afterCall records the outcome of a call and updates the breaker state.
func (cb *CircuitBreaker) afterCall(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if err != nil {
		cb.failures++
		cb.lastFailure = time.Now()
		if cb.state == StateHalfOpen || cb.failures >= cb.threshold {
			cb.state = StateOpen
		}
		cb.halfOpenProbing = false
		return
	}

	// Success: recover fully.
	cb.failures = 0
	cb.state = StateClosed
	cb.halfOpenProbing = false
}

// State returns the current state, applying the Open→HalfOpen transition if the
// reset timeout has elapsed.
func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.state == StateOpen && time.Since(cb.lastFailure) >= cb.resetTimeout {
		cb.state = StateHalfOpen
	}
	return cb.state
}

// Reset returns the breaker to the closed state and clears the failure count.
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.state = StateClosed
	cb.failures = 0
	cb.lastFailure = time.Time{}
}
