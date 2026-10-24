package model

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestWaitForRetryCanceledAndTimerReady(t *testing.T) {
	for _, delay := range []time.Duration{0, -time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				// Both select cases are ready; either selection must report cancellation.
				for range 100 {
					if err := waitForRetry(ctx, delay); !errors.Is(err, context.Canceled) {
						t.Fatalf("waitForRetry error = %v, want context.Canceled", err)
					}
				}
			})
		})
	}
}
