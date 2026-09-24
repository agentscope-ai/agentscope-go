package inference

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func httpDeployment(t *testing.T, url string, attempts int) (*Deployment, *Ledger) {
	t.Helper()
	l := NewLedger(20)
	c := NewController(l)
	t.Cleanup(c.Close)
	if err := c.RegisterPool(PoolConfig{ID: "p", MaxActive: 1, MaxQueued: 1, MaxQueuedWork: 10}); err != nil {
		t.Fatal(err)
	}
	d, err := c.Register(&DeploymentConfig{Descriptor: Descriptor{ID: "d", Provider: "fixture", API: "openai-chat", Model: "m", Endpoint: url}, PoolID: "p", MaxAttempts: attempts, Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return d, l
}
func TestHTTPAttemptsAndUnknownUsage(t *testing.T) {
	var n atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) < 2 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
	}))
	defer server.Close()
	d, l := httpDeployment(t, server.URL, 2)
	ctx := WithIdentity(context.Background(), Identity{TenantID: "host"})
	ctx, finish := d.Start(ctx, "reasoning")
	var out map[string]any
	err := CurrentOperation(ctx).DoJSON(ctx, server.Client(), "POST", server.URL, map[string]string{"model": "m"}, &out, nil)
	finish(err)
	if err != nil {
		t.Fatal(err)
	}
	s := l.Snapshot()
	if len(s.Attempts) != 2 || !s.Attempts[1].UsageKnown || s.Attempts[0].UsageKnown || s.Attempts[0].Status != 503 {
		t.Fatalf("invalid attempt facts: %+v", s)
	}
	if n.Load() != 2 {
		t.Fatal(n.Load())
	}
}
func TestHTTPIdentityAndRetryAfterCancellation(t *testing.T) {
	arrived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		arrived <- struct{}{}
	}))
	defer server.Close()
	d, l := httpDeployment(t, server.URL, 3)
	ctx, finish := d.Start(context.Background(), "reasoning")
	err := CurrentOperation(ctx).DoJSON(ctx, server.Client(), "POST", server.URL, struct{}{}, nil, nil)
	finish(err)
	if !errors.Is(err, ErrIdentityRequired) || len(l.Snapshot().Attempts) != 0 {
		t.Fatalf("missing identity sent: %v", err)
	}
	ctx, cancel := context.WithCancel(WithIdentity(context.Background(), Identity{TenantID: "host"}))
	defer cancel()
	ctx, finish = d.Start(ctx, "reasoning")
	done := make(chan error, 1)
	go func() {
		err := CurrentOperation(ctx).DoJSON(ctx, server.Client(), "POST", server.URL, struct{}{}, nil, nil)
		finish(err)
		done <- err
	}()
	<-arrived
	deadline := time.Now().Add(time.Second)
	for d.Stats().Active != 0 {
		if time.Now().After(deadline) {
			t.Fatal("retry backoff retained permit")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if s := l.Snapshot(); len(s.Attempts) != 1 {
		t.Fatalf("canceled retry sent again: %+v", s)
	}
}

func TestHTTPClientTimeoutAndTruncatedStatusRetry(t *testing.T) {
	for _, failure := range []string{"timeout", "truncated503"} {
		t.Run(failure, func(t *testing.T) {
			var sends atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if sends.Add(1) == 1 {
					if failure == "timeout" {
						time.Sleep(100 * time.Millisecond)
						return
					}
					w.Header().Set("Content-Length", "100")
					w.WriteHeader(503)
					w.Write([]byte("x"))
					return
				}
				w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			}))
			defer server.Close()
			d, _ := httpDeployment(t, server.URL, 2)
			client := server.Client()
			if failure == "timeout" {
				client.Timeout = 30 * time.Millisecond
			}
			ctx, finish := d.Start(WithIdentity(context.Background(), Identity{TenantID: "host"}), "reasoning")
			err := CurrentOperation(ctx).DoJSON(ctx, client, "POST", server.URL, struct{}{}, nil, nil)
			finish(err)
			if err != nil || sends.Load() != 2 {
				t.Fatalf("retry lost for %s: sends=%d err=%v", failure, sends.Load(), err)
			}
		})
	}
}
