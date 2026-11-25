package jev_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/jev"
)

func TestModelOverride(t *testing.T) {
	models := make(chan string, 3)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		models <- payload.Model
		fmt.Fprint(w, fixture)
	}))
	defer s.Close()
	cl := client(t, s.URL)
	ctx := context.Background()
	for _, tc := range []struct {
		view    *jev.Client
		want    string
		managed bool
	}{
		{cl.WithModel("alternate"), "alternate", false},
		{cl, "requested-version", false},
		{cl.WithModel("requested-version"), "requested-version", true},
	} {
		var out *c.Response
		var err error
		if tc.managed {
			var began bool
			out, err = tc.view.ClassifyWithDispatch(ctx, request(), c.DispatchLimits{}, func(c.AttemptKind) error { began = true; return nil })
			if !began {
				t.Error("matching override skipped guard")
			}
		} else {
			out, err = tc.view.Classify(ctx, request())
		}
		if err != nil || len(out.Attempts) != 1 {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		if wire := <-models; wire != tc.want || out.Attempts[0].RequestedModel != tc.want || out.Model != "jev-fixture-v1" || out.Attempts[0].Model != out.Model {
			t.Errorf("want requested model %q, wire=%q report=%+v", tc.want, wire, out)
		}
		if endpoint, model := tc.view.Target(); endpoint != s.URL || model != "requested-version" {
			t.Fatalf("changed target: %q, %q", endpoint, model)
		}
	}
}

func TestModelOverrideRejectedBeforeDispatch(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, fixture)
	}))
	defer s.Close()
	cl := client(t, s.URL)
	for _, tc := range []struct {
		model          string
		managed, guard bool
	}{
		{"alternate", true, true}, {"alternate", true, false},
		{"", false, false}, {"  ", false, false}, {"\xff", false, false},
	} {
		view := cl.WithModel(tc.model)
		ctx := context.Background()
		var out *c.Response
		var err error
		if tc.managed {
			var begin func(c.AttemptKind) error
			if tc.guard {
				begin = func(c.AttemptKind) error { t.Error("rejected override invoked guard"); return nil }
			}
			out, err = view.ClassifyWithDispatch(ctx, request(), c.DispatchLimits{}, begin)
		} else {
			out, err = view.Classify(ctx, request())
		}
		if !errors.Is(err, c.ErrInvalidRequest) || out == nil || out.OperationID != "op" || len(out.Attempts) != 0 || calls.Load() != 0 {
			t.Errorf("model=%q managed=%t: err=%v attempts=%+v http calls=%d", tc.model, tc.managed, err, out, calls.Load())
		}
	}
}

func TestModelViewsInParallel(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string
			State string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != body.State {
			t.Errorf("unexpected model for call: %+v, err=%v", body, err)
		}
		fmt.Fprint(w, fixture)
	}))
	t.Cleanup(s.Close)
	cl := client(t, s.URL)
	first := cl.WithModel("first")
	for name, view := range map[string]*jev.Client{"first": first, "second": first.WithModel("second"), "requested-version": cl} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, model := view.Target(); model != "requested-version" {
				t.Fatalf("target model = %q", model)
			}
			req := request()
			req.State, _ = json.Marshal(name)
			out, err := view.Classify(context.Background(), req)
			if err != nil || len(out.Attempts) != 1 || out.Attempts[0].RequestedModel != name {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
}

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
