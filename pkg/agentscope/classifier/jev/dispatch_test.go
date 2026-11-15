package jev_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
)

func TestManagedDispatchGuardAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		limits   c.DispatchLimits
		guard    bool
		want     error
		attempts int
	}{
		{name: "guard", guard: true, want: context.Canceled},
		{name: "negative-request", limits: c.DispatchLimits{MaxRequestBytes: -1}, want: c.ErrInvalidRequest},
		{name: "negative-response", limits: c.DispatchLimits{MaxResponseBytes: -1}, want: c.ErrInvalidRequest},
		{name: "overflow-response", limits: c.DispatchLimits{MaxResponseBytes: math.MaxInt64}, want: c.ErrInvalidRequest},
		{name: "request-size", limits: c.DispatchLimits{MaxRequestBytes: 1}, want: c.ErrInvalidRequest},
		{name: "response-size", limits: c.DispatchLimits{MaxResponseBytes: 1}, want: c.ErrInvalidResponse, attempts: 1},
		{name: "success", limits: c.DispatchLimits{MaxRequestBytes: 4096, MaxResponseBytes: 4096}, attempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); fmt.Fprint(w, fixture) }))
			defer server.Close()
			cl := client(t, server.URL)
			endpoint, version := cl.Target()
			if endpoint != server.URL || version != "requested-version" {
				t.Fatal("wrong binding")
			}
			out, err := cl.ClassifyWithDispatch(context.Background(), request(), tc.limits, func(kind c.AttemptKind) error {
				if kind != c.PhysicalAttempt {
					t.Fatal("HTTP marked simulated")
				}
				if tc.guard {
					return context.Canceled
				}
				return nil
			})
			if !errors.Is(err, tc.want) || len(out.Attempts) != tc.attempts || calls.Load() != int32(tc.attempts) {
				t.Fatalf("out=%+v err=%v calls=%d", out, err, calls.Load())
			}
		})
	}
}
