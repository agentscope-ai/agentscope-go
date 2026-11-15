package resilience

import (
	"context"
	"errors"
	"sync"
	"time"
)

// RateLimiter is a token bucket limiter using only the standard library.
// Tokens refill at rate per second up to burst capacity.
type RateLimiter struct {
	rate  float64 // tokens per second
	burst float64 // bucket capacity

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// NewRateLimiter creates a token bucket that refills at rate tokens/second with
// a maximum of burst tokens. The bucket starts full.
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	if rate < 0 {
		rate = 0
	}
	if burst < 0 {
		burst = 0
	}
	return &RateLimiter{
		rate:   rate,
		burst:  float64(burst),
		tokens: float64(burst),
		last:   time.Now(),
	}
}

// refill adds tokens accrued since the last update. Caller must hold cb.mu.
func (r *RateLimiter) refill(now time.Time) {
	elapsed := now.Sub(r.last).Seconds()
	if elapsed <= 0 {
		return
	}
	r.last = now
	r.tokens += elapsed * r.rate
	if r.tokens > r.burst {
		r.tokens = r.burst
	}
}

// Allow reports whether a token is available right now, consuming one if so.
// It never blocks.
func (r *RateLimiter) Allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refill(time.Now())
	if r.tokens >= 1 {
		r.tokens--
		return true
	}
	return false
}

// Wait blocks until a token is available or ctx is done. It returns ctx.Err()
// if the context is canceled before a token becomes available.
func (r *RateLimiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for {
		wait, ok := r.reserve()
		if ok {
			return nil
		}
		if wait <= 0 {
			// Refill disabled (rate 0) and no tokens: cannot ever proceed.
			return errors.New("rate limiter: no tokens available and refill is disabled")
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// reserve consumes a token if available, returning (0, true). Otherwise it
// returns the estimated wait until the next token, and false. A non-positive
// wait with ok=false means refilling is disabled.
func (r *RateLimiter) reserve() (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refill(time.Now())
	if r.tokens >= 1 {
		r.tokens--
		return 0, true
	}
	if r.rate <= 0 {
		return 0, false
	}
	needed := 1 - r.tokens
	return time.Duration(needed / r.rate * float64(time.Second)), false
}
