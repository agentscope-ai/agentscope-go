package bench

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestOpenLoopOffersWhileCallbacksAreBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		report, err := NewRunner().RunOpenLoop(context.Background(), &OpenLoopScenario{
			ArrivalOffsets: []time.Duration{0, time.Second, 2 * time.Second, 3 * time.Second},
			MaxInFlight:    1,
			Timeout:        10 * time.Second,
			DrainTimeout:   10 * time.Second,
			Run: func(context.Context, int) error {
				calls.Add(1)
				time.Sleep(4 * time.Second)
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []OpenLoopStatus{OpenLoopSucceeded, OpenLoopRejected, OpenLoopRejected, OpenLoopRejected}
		assertOpenLoopStatuses(t, report, want)
		if report.Offered != 4 || calls.Load() != 1 {
			t.Fatalf("offered=%d callbacks=%d; want 4 offered, 1 callback", report.Offered, calls.Load())
		}
		if report.Results[0].Latency != 4*time.Second {
			t.Fatalf("latency=%v; want scheduled-arrival to completion", report.Results[0].Latency)
		}
		for i, result := range report.Results {
			if result.Iteration != i+1 || !result.ScheduledAt.Equal(report.StartTime.Add(time.Duration(i)*time.Second)) {
				t.Fatalf("arrival %d lost its identity or schedule: %+v", i, result)
			}
			if i > 0 && (!result.StartedAt.IsZero() || !result.FinishedAt.IsZero() || result.Latency != 0) {
				t.Fatalf("rejected arrival has fabricated execution timestamps: %+v", result)
			}
		}
	})
}

func TestOpenLoopBurstBoundsCallbacks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		report, err := NewRunner().RunOpenLoop(context.Background(), &OpenLoopScenario{
			ArrivalOffsets: []time.Duration{0, 0, 0, 0, 0},
			MaxInFlight:    2,
			Timeout:        10 * time.Second,
			DrainTimeout:   10 * time.Second,
			Run: func(context.Context, int) error {
				calls.Add(1)
				time.Sleep(time.Second)
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopSucceeded, OpenLoopSucceeded, OpenLoopRejected, OpenLoopRejected, OpenLoopRejected})
		if calls.Load() != 2 {
			t.Fatalf("callbacks=%d; want 2", calls.Load())
		}
	})
}

func TestOpenLoopTimeoutDoesNotReleaseRunningCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		report, err := NewRunner().RunOpenLoop(context.Background(), &OpenLoopScenario{
			ArrivalOffsets: []time.Duration{0, 2 * time.Second, 4 * time.Second},
			MaxInFlight:    1,
			Timeout:        time.Second,
			DrainTimeout:   5 * time.Second,
			Run: func(ctx context.Context, iteration int) error {
				calls.Add(1)
				if iteration == 1 {
					<-ctx.Done()
					time.Sleep(2 * time.Second) // Deliberately ignore cancellation temporarily.
					return nil
				}
				return errors.New("fixture failure")
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopTimedOut, OpenLoopRejected, OpenLoopFailed})
		if calls.Load() != 2 || report.Results[0].Latency != 3*time.Second || report.Results[2].Error != "fixture failure" {
			t.Fatalf("unexpected outcomes: %+v; callbacks=%d", report.Results, calls.Load())
		}
	})
}

func TestOpenLoopCancellationRetainsAllPlannedArrivals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			time.Sleep(time.Second)
			cancel()
		}()
		report, err := NewRunner().RunOpenLoop(ctx, &OpenLoopScenario{
			ArrivalOffsets: []time.Duration{0, 2 * time.Second},
			MaxInFlight:    1,
			Timeout:        5 * time.Second,
			DrainTimeout:   5 * time.Second,
			Run: func(context.Context, int) error {
				time.Sleep(500 * time.Millisecond)
				return nil
			},
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v; want context cancellation", err)
		}
		assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopSucceeded, OpenLoopNotOffered})
		if report.Offered != 1 || report.EndTime.Sub(report.StartTime) != time.Second {
			t.Fatalf("unexpected partial report: %+v", report)
		}
	})
}

func TestOpenLoopDrainReturnsDetachedSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		done := make(chan struct{})
		defer close(release)
		contexts := make(chan context.Context, 1)
		report, err := NewRunner().RunOpenLoop(context.Background(), &OpenLoopScenario{
			ArrivalOffsets: []time.Duration{0},
			MaxInFlight:    1,
			Timeout:        10 * time.Second,
			DrainTimeout:   time.Second,
			Run: func(ctx context.Context, _ int) error {
				contexts <- ctx
				<-release // Simulate an uncooperative callback, then clean it up below.
				close(done)
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopUnfinished})
		if report.EndTime.Sub(report.StartTime) != time.Second || !report.Results[0].FinishedAt.IsZero() {
			t.Fatalf("unexpected drain outcome: %+v", report)
		}
		if (<-contexts).Err() == nil {
			t.Fatal("drain did not cancel callback context")
		}
		before := append([]OpenLoopResult(nil), report.Results...)
		release <- struct{}{}
		<-done
		synctest.Wait()
		if !reflect.DeepEqual(before, report.Results) {
			t.Fatalf("late completion mutated published snapshot: before=%+v after=%+v", before, report.Results)
		}
	})
}

func TestOpenLoopCallbackCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		report, err := NewRunner().RunOpenLoop(context.Background(), &OpenLoopScenario{
			ArrivalOffsets: []time.Duration{0}, MaxInFlight: math.MaxInt,
			Timeout: time.Second, DrainTimeout: time.Second,
			Run: func(context.Context, int) error { return context.Canceled },
		})
		if err != nil {
			t.Fatal(err)
		}
		assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopCanceled})
	})
}

func TestOpenLoopWrappedCallbackErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status OpenLoopStatus
	}{
		{"deadline", errors.Join(errors.New("callback deadline"), context.DeadlineExceeded), OpenLoopTimedOut},
		{"cancellation", errors.Join(errors.New("callback canceled"), context.Canceled), OpenLoopCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				report, err := NewRunner().RunOpenLoop(context.Background(), &OpenLoopScenario{
					ArrivalOffsets: []time.Duration{0}, MaxInFlight: 1,
					Timeout: time.Second, DrainTimeout: time.Second,
					Run: func(context.Context, int) error { return tc.err },
				})
				if err != nil {
					t.Fatal(err)
				}
				assertOpenLoopStatuses(t, report, []OpenLoopStatus{tc.status})
				if report.Results[0].Error != tc.err.Error() {
					t.Fatalf("callback error detail lost: %+v", report.Results[0])
				}
			})
		})
	}
}

func TestOpenLoopDoesNotMutateScenario(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scenario := &OpenLoopScenario{
			Name: "fixture", ArrivalOffsets: []time.Duration{time.Second, 2 * time.Second},
			MaxInFlight: 1, Timeout: time.Second, DrainTimeout: time.Second,
			Run: func(context.Context, int) error { return nil },
		}
		report, err := NewRunner().RunOpenLoop(context.Background(), scenario)
		if err != nil {
			t.Fatal(err)
		}
		assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopSucceeded, OpenLoopSucceeded})
		if scenario.Name != "fixture" || scenario.MaxInFlight != 1 || scenario.Timeout != time.Second || scenario.DrainTimeout != time.Second || !reflect.DeepEqual(scenario.ArrivalOffsets, []time.Duration{time.Second, 2 * time.Second}) {
			t.Fatalf("caller configuration changed: %+v", scenario)
		}
		if report.Scenario != scenario.Name || report.Offered != 2 || report.Results[0].Latency != 0 {
			t.Fatalf("unexpected successful report: %+v", report)
		}
	})
}

func TestOpenLoopDeadlineBoundary(t *testing.T) {
	for _, returned := range []error{nil, context.Canceled, errors.New("late failure")} {
		synctest.Test(t, func(t *testing.T) {
			report, err := NewRunner().RunOpenLoop(context.Background(), &OpenLoopScenario{
				ArrivalOffsets: []time.Duration{0}, MaxInFlight: 1,
				Timeout: time.Second, DrainTimeout: 2 * time.Second,
				Run: func(context.Context, int) error {
					time.Sleep(time.Second)
					return returned
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopTimedOut})
		})
	}
}

func TestOpenLoopDrainBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		report, err := NewRunner().RunOpenLoop(context.Background(), &OpenLoopScenario{
			ArrivalOffsets: []time.Duration{0}, MaxInFlight: 1,
			Timeout: 2 * time.Second, DrainTimeout: time.Second,
			Run: func(context.Context, int) error {
				time.Sleep(time.Second)
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopUnfinished})
	})
}

func TestOpenLoopAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := NewRunner().RunOpenLoop(ctx, &OpenLoopScenario{
		ArrivalOffsets: []time.Duration{0, time.Hour}, MaxInFlight: 1,
		Timeout: time.Second, DrainTimeout: time.Second,
		Run: func(context.Context, int) error { t.Fatal("canceled run invoked callback"); return nil },
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v; want cancellation", err)
	}
	assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopNotOffered, OpenLoopNotOffered})
	if report.Offered != 0 || !report.EndTime.Equal(report.StartTime) {
		t.Fatalf("pre-canceled caller started observation: %+v", report)
	}
}

type stalledOpenLoopError struct {
	release <-chan struct{}
}

func (e stalledOpenLoopError) Error() string {
	<-e.release
	return "delayed error text"
}

func TestOpenLoopErrorFormattingDoesNotBlockDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		report, err := NewRunner().RunOpenLoop(context.Background(), &OpenLoopScenario{
			ArrivalOffsets: []time.Duration{0, time.Second}, MaxInFlight: 1,
			Timeout: 10 * time.Second, DrainTimeout: time.Second,
			Run: func(context.Context, int) error { return stalledOpenLoopError{release: release} },
		})
		if err != nil {
			t.Fatal(err)
		}
		assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopUnfinished, OpenLoopRejected})
		if report.EndTime.Sub(report.StartTime) != 2*time.Second {
			t.Fatalf("error normalization blocked observation: %+v", report)
		}
	})
}

func TestOpenLoopDelayedScheduler(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		origin := time.Now().Add(-2 * time.Second)
		report, err := runOpenLoop(context.Background(), OpenLoopScenario{
			ArrivalOffsets: []time.Duration{0, 3 * time.Second}, MaxInFlight: 1,
			Timeout: time.Second, DrainTimeout: time.Second,
			Run: func(context.Context, int) error { return nil },
		}, origin)
		if err != nil {
			t.Fatal(err)
		}
		assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopTimedOut, OpenLoopSucceeded})
		if !report.Results[0].StartedAt.IsZero() || !report.Results[0].ScheduledAt.Equal(origin) {
			t.Fatalf("overdue arrival was executed or rescheduled: %+v", report.Results[0])
		}
	})
}

func TestOpenLoopDispatchLagCountsInLatency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		origin := time.Now().Add(-time.Second)
		report, err := runOpenLoop(context.Background(), OpenLoopScenario{
			ArrivalOffsets: []time.Duration{0}, MaxInFlight: 1,
			Timeout: 5 * time.Second, DrainTimeout: 5 * time.Second,
			Run: func(context.Context, int) error { time.Sleep(time.Second); return nil },
		}, origin)
		if err != nil {
			t.Fatal(err)
		}
		assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopSucceeded})
		if report.Results[0].Latency != 2*time.Second {
			t.Fatalf("latency=%v; dispatch delay was omitted", report.Results[0].Latency)
		}
	})
}

func TestOpenLoopSchedulerStartsAfterCutoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		origin := time.Now().Add(-5 * time.Second)
		report, err := runOpenLoop(context.Background(), OpenLoopScenario{
			ArrivalOffsets: []time.Duration{0, time.Second}, MaxInFlight: 1,
			Timeout: 10 * time.Second, DrainTimeout: time.Second,
			Run: func(context.Context, int) error { t.Fatal("callback started after cutoff"); return nil },
		}, origin)
		if err != nil {
			t.Fatal(err)
		}
		assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopTimedOut, OpenLoopTimedOut})
		if report.Offered != 2 || !report.EndTime.Equal(origin.Add(2*time.Second)) {
			t.Fatalf("invalid late-scheduler report: %+v", report)
		}
	})
}

func TestOpenLoopCallerDeadlineBoundsObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		report, err := NewRunner().RunOpenLoop(ctx, &OpenLoopScenario{
			ArrivalOffsets: []time.Duration{0, 2 * time.Second}, MaxInFlight: 1,
			Timeout: 10 * time.Second, DrainTimeout: 10 * time.Second,
			Run: func(context.Context, int) error { time.Sleep(time.Second); return nil },
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error=%v; want caller deadline", err)
		}
		assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopUnfinished, OpenLoopNotOffered})
		if report.EndTime.Sub(report.StartTime) != time.Second {
			t.Fatalf("parent deadline did not bound observation: %+v", report)
		}
	})
}

// Model a context whose deadline timer notification is delayed. Deadline is
// authoritative even while Err and Done still report no cancellation.
type delayedDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c delayedDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func TestOpenLoopDelayedParentDeadlineNotification(t *testing.T) {
	for _, remaining := range []time.Duration{0, time.Second} {
		synctest.Test(t, func(t *testing.T) {
			ctx := delayedDeadlineContext{Context: context.Background(), deadline: time.Now().Add(remaining)}
			var calls atomic.Int64
			report, err := NewRunner().RunOpenLoop(ctx, &OpenLoopScenario{
				ArrivalOffsets: []time.Duration{0, time.Second, 2 * time.Second}, MaxInFlight: 1,
				Timeout: 10 * time.Second, DrainTimeout: 10 * time.Second,
				Run: func(context.Context, int) error {
					calls.Add(1)
					time.Sleep(time.Second)
					return nil
				},
			})
			if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				t.Fatalf("error=%v parent=%v; deadline must not depend on parent notification", err, ctx.Err())
			}
			if remaining == 0 {
				assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopNotOffered, OpenLoopNotOffered, OpenLoopNotOffered})
				if calls.Load() != 0 || report.Offered != 0 {
					t.Fatalf("already elapsed deadline allowed work: %+v", report)
				}
			} else {
				assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopUnfinished, OpenLoopCanceled, OpenLoopNotOffered})
				if calls.Load() != 1 {
					t.Fatalf("callbacks=%d; want only first arrival", calls.Load())
				}
			}
			if !report.EndTime.Equal(ctx.deadline) {
				t.Fatalf("end=%v; want parent deadline %v", report.EndTime, ctx.deadline)
			}
		})
	}
}

func TestOpenLoopCancellationSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release := make(chan struct{})
		go func() { time.Sleep(time.Second); cancel() }()
		report, err := NewRunner().RunOpenLoop(ctx, &OpenLoopScenario{
			ArrivalOffsets: []time.Duration{0}, MaxInFlight: 1,
			Timeout: 10 * time.Second, DrainTimeout: 10 * time.Second,
			Run: func(context.Context, int) error { <-release; return nil },
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v; want cancellation", err)
		}
		assertOpenLoopStatuses(t, report, []OpenLoopStatus{OpenLoopUnfinished})
		before := append([]OpenLoopResult(nil), report.Results...)
		close(release)
		synctest.Wait()
		if !reflect.DeepEqual(before, report.Results) {
			t.Fatal("late callback changed canceled report")
		}
	})
}

func TestOpenLoopRejectsInvalidConfiguration(t *testing.T) {
	valid := func() *OpenLoopScenario {
		return &OpenLoopScenario{
			ArrivalOffsets: []time.Duration{0}, MaxInFlight: 1,
			Timeout: time.Second, DrainTimeout: time.Second,
			Run: func(context.Context, int) error { t.Fatal("invalid scenario invoked callback"); return nil },
		}
	}
	cases := []struct {
		name string
		edit func(*OpenLoopScenario)
	}{
		{"nil callback", func(s *OpenLoopScenario) { s.Run = nil }},
		{"empty schedule", func(s *OpenLoopScenario) { s.ArrivalOffsets = nil }},
		{"negative arrival", func(s *OpenLoopScenario) { s.ArrivalOffsets = []time.Duration{-1} }},
		{"unordered schedule", func(s *OpenLoopScenario) { s.ArrivalOffsets = []time.Duration{2, 1} }},
		{"zero limit", func(s *OpenLoopScenario) { s.MaxInFlight = 0 }},
		{"negative limit", func(s *OpenLoopScenario) { s.MaxInFlight = -1 }},
		{"zero timeout", func(s *OpenLoopScenario) { s.Timeout = 0 }},
		{"negative timeout", func(s *OpenLoopScenario) { s.Timeout = -1 }},
		{"zero drain", func(s *OpenLoopScenario) { s.DrainTimeout = 0 }},
		{"negative drain", func(s *OpenLoopScenario) { s.DrainTimeout = -1 }},
		{"drain overflow", func(s *OpenLoopScenario) { s.ArrivalOffsets = []time.Duration{math.MaxInt64}; s.Timeout = 1 }},
		{"deadline overflow", func(s *OpenLoopScenario) { s.ArrivalOffsets = []time.Duration{math.MaxInt64 - 1}; s.DrainTimeout = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scenario := valid()
			tc.edit(scenario)
			if report, err := NewRunner().RunOpenLoop(context.Background(), scenario); err == nil || report != nil {
				t.Fatalf("report=%+v error=%v; want configuration error and nil report", report, err)
			}
		})
	}
	if report, err := NewRunner().RunOpenLoop(context.Background(), nil); err == nil || report != nil {
		t.Fatalf("nil scenario: report=%+v error=%v", report, err)
	}
	if report, err := NewRunner().RunOpenLoop(nil, valid()); err == nil || report != nil { //nolint:staticcheck // Exercise explicit nil-context validation.
		t.Fatalf("nil context: report=%+v error=%v", report, err)
	}
}

func assertOpenLoopStatuses(t *testing.T, report *OpenLoopReport, want []OpenLoopStatus) {
	t.Helper()
	if report == nil || len(report.Results) != len(want) {
		t.Fatalf("report=%+v; want %d result records", report, len(want))
	}
	for i, result := range report.Results {
		if result.Status != want[i] {
			t.Errorf("arrival %d: status=%q; want %q (record=%+v)", i+1, result.Status, want[i], result)
		}
	}
}
