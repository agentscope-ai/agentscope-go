package resilience

import (
	"context"
	"testing"
	"time"
)

func TestRateLimiterAllowBurst(t *testing.T) {
	rl := NewRateLimiter(1, 3)
	for i := 0; i < 3; i++ {
		if !rl.Allow() {
			t.Fatalf("call %d: expected allow within burst", i)
		}
	}
	if rl.Allow() {
		t.Fatal("expected deny after burst exhausted")
	}
}

func TestRateLimiterRefills(t *testing.T) {
	rl := NewRateLimiter(100, 1) // 100 tokens/sec
	if !rl.Allow() {
		t.Fatal("expected first token")
	}
	if rl.Allow() {
		t.Fatal("expected empty bucket")
	}
	time.Sleep(30 * time.Millisecond) // ~3 tokens refilled
	if !rl.Allow() {
		t.Fatal("expected refilled token")
	}
}

func TestRateLimiterWait(t *testing.T) {
	rl := NewRateLimiter(50, 1) // 50/sec → 20ms per token
	if err := rl.Wait(context.Background()); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	start := time.Now()
	if err := rl.Wait(context.Background()); err != nil {
		t.Fatalf("second wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Fatalf("expected wait to block for refill, only waited %v", elapsed)
	}
}

func TestRateLimiterWaitContextCancel(t *testing.T) {
	rl := NewRateLimiter(0.001, 1) // effectively never refills in test window
	_ = rl.Allow()                 // drain the single token
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := rl.Wait(ctx); err == nil {
		t.Fatal("expected context error")
	}
}

func TestRateLimiterWaitAlreadyCanceled(t *testing.T) {
	rl := NewRateLimiter(10, 5)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rl.Wait(ctx); err == nil {
		t.Fatal("expected error for canceled context")
	}
}
