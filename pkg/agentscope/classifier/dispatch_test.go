package classifier_test

import (
	"context"
	"errors"
	"testing"

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
)

func TestFakeDispatchGuardAndLimits(t *testing.T) {
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
		{name: "request-size", limits: c.DispatchLimits{MaxRequestBytes: 1}, want: c.ErrInvalidRequest},
		{name: "response-size", limits: c.DispatchLimits{MaxResponseBytes: 1}, want: c.ErrInvalidResponse, attempts: 1},
		{name: "success", limits: c.DispatchLimits{MaxRequestBytes: 4096, MaxResponseBytes: 4096}, attempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := c.NewFake(&c.FakeConfig{Response: response()})
			endpoint, version := f.Target()
			if endpoint != "fake://classifier" || version != "fixture-v1" {
				t.Fatal("wrong fixture binding")
			}
			begin := func(kind c.AttemptKind) error {
				if kind != c.SimulatedAttempt {
					t.Fatal("fake marked physical")
				}
				if tc.guard {
					return context.Canceled
				}
				return nil
			}
			out, err := f.ClassifyWithDispatch(context.Background(), request(), tc.limits, begin)
			if !errors.Is(err, tc.want) || len(out.Attempts) != tc.attempts {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			select {
			case <-f.Started():
				if tc.attempts == 0 {
					t.Fatal("rejected dispatch started")
				}
			default:
				if tc.attempts > 0 {
					t.Fatal("missing simulated dispatch")
				}
			}
		})
	}
}
