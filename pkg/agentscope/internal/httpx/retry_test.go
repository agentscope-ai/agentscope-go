package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestDoJSONRequest_Retries429 proves 429 (Too Many Requests) is retried. The old
// logic only retried 5xx, so a rate-limited request failed after a single attempt.
func TestDoJSONRequest_Retries429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	var out map[string]any
	err := DoJSONRequest(context.Background(), srv.Client(), http.MethodPost, srv.URL, map[string]any{}, &out, nil)
	if err != nil {
		t.Fatalf("expected success after 429 retry, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 calls (429 then 200), got %d", got)
	}
}

// TestDoJSONRequest_CancelDuringBackoff proves the retry backoff honors context
// cancellation instead of sleeping through it.
func TestDoJSONRequest_CancelDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60") // force a long backoff before the next attempt
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := DoJSONRequest(ctx, srv.Client(), http.MethodPost, srv.URL, map[string]any{}, nil, nil)
	if err == nil {
		t.Fatal("expected error on cancellation")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("backoff did not honor ctx cancellation (took %v)", elapsed)
	}
}
