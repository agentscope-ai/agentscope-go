package bench

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// OpenLoopScenario offers callbacks on a finite schedule independent of their
// completion. Configuration is copied before execution; callers must not mutate
// it concurrently with that copy. This API is experimental.
type OpenLoopScenario struct {
	Name           string
	ArrivalOffsets []time.Duration // Nonempty, nonnegative, nondecreasing; duplicates form bursts.
	MaxInFlight    int             // Positive bound on callback wrappers, including error inspection.
	Timeout        time.Duration   // Positive per-arrival timeout, measured from scheduled arrival.
	DrainTimeout   time.Duration   // Positive observation allowance after the last scheduled arrival.
	Run            func(context.Context, int) error
}

// OpenLoopStatus describes the observed outcome of one planned arrival.
type OpenLoopStatus string

const (
	OpenLoopSucceeded  OpenLoopStatus = "succeeded"   // Completion before the deadline, without error.
	OpenLoopFailed     OpenLoopStatus = "failed"      // Completion with an ordinary callback error.
	OpenLoopRejected   OpenLoopStatus = "rejected"    // Generator's in-flight limit was full.
	OpenLoopTimedOut   OpenLoopStatus = "timed_out"   // Arrival, callback, or observation deadline expired.
	OpenLoopCanceled   OpenLoopStatus = "canceled"    // Callback cancellation or interrupted due arrival.
	OpenLoopUnfinished OpenLoopStatus = "unfinished"  // Authorized wrapper lacked a recorded completion.
	OpenLoopNotOffered OpenLoopStatus = "not_offered" // Observation did not reach the planned arrival.
)

// OpenLoopResult retains one planned arrival. StartedAt is synchronized start
// authorization, not physical callback entry. FinishedAt is accepted wrapper
// completion, including returned-error inspection. Latency includes dispatch lag.
// Unstarted records have zero StartedAt; records without accepted completion have
// zero FinishedAt and Latency. Iteration is one-based.
type OpenLoopResult struct {
	Iteration   int
	ScheduledAt time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
	Latency     time.Duration
	Status      OpenLoopStatus
	Error       string
}

// OpenLoopReport is a snapshot in original schedule order. Offered counts
// arrivals reached by observation, including rejection and timeout. Results also
// retains future, not-offered arrivals. The runner never mutates a returned report.
type OpenLoopReport struct {
	Scenario  string
	StartTime time.Time
	EndTime   time.Time
	Offered   int
	Results   []OpenLoopResult
}

// RunOpenLoop offers work at absolute offsets from observation start. It rejects
// arrivals when MaxInFlight is occupied instead of queueing or delaying them.
// Overdue arrivals keep their original schedule and are recorded as timed out.
//
// Observation ends after the final arrival and all wrapper completions, at the
// drain deadline, or on caller cancellation/deadline. Caller interruption returns
// a partial report and its context error; drain expiry returns a report and nil.
// An already-canceled caller leaves all planned arrivals not offered.
//
// Run and returned-error methods must return promptly on cancellation. A wrapper
// owns its slot through error inspection and result publication. Uncooperative
// wrappers (at most MaxInFlight per invocation) may outlive observation; the caller
// must arrange their termination before repeated runs. A preempted wrapper can
// enter Run after authorization with a canceled context. No remote concurrency or
// task-quality guarantee follows from these local callback measurements.
func (r *Runner) RunOpenLoop(ctx context.Context, scenario *OpenLoopScenario) (*OpenLoopReport, error) {
	if ctx == nil {
		return nil, fmt.Errorf("bench: context must not be nil")
	}
	cfg, err := validateOpenLoop(scenario)
	if err != nil {
		return nil, err
	}
	return runOpenLoop(ctx, cfg, time.Now())
}

func validateOpenLoop(scenario *OpenLoopScenario) (OpenLoopScenario, error) {
	if scenario == nil || scenario.Run == nil {
		return OpenLoopScenario{}, fmt.Errorf("bench: open-loop scenario and Run must not be nil")
	}
	cfg := *scenario
	if len(cfg.ArrivalOffsets) == 0 || cfg.MaxInFlight <= 0 || cfg.Timeout <= 0 || cfg.DrainTimeout <= 0 {
		return OpenLoopScenario{}, fmt.Errorf("bench: open-loop schedule must be nonempty and limits must be positive")
	}
	cfg.ArrivalOffsets = append([]time.Duration(nil), cfg.ArrivalOffsets...)
	var previous time.Duration
	for _, offset := range cfg.ArrivalOffsets {
		if offset < previous {
			return OpenLoopScenario{}, fmt.Errorf("bench: arrival offsets must be nonnegative and nondecreasing")
		}
		previous = offset
	}
	if previous > math.MaxInt64-cfg.Timeout || previous > math.MaxInt64-cfg.DrainTimeout {
		return OpenLoopScenario{}, fmt.Errorf("bench: open-loop deadline overflows time.Duration")
	}
	return cfg, nil
}

// runOpenLoop takes an origin separately so tests can reproduce scheduler lag
// against the same absolute schedule used in production.
func runOpenLoop(ctx context.Context, cfg OpenLoopScenario, origin time.Time) (*OpenLoopReport, error) {
	report := &OpenLoopReport{
		Scenario: cfg.Name, StartTime: origin, EndTime: origin,
		Results: make([]OpenLoopResult, len(cfg.ArrivalOffsets)),
	}
	for i, offset := range cfg.ArrivalOffsets {
		report.Results[i] = OpenLoopResult{
			Iteration: i + 1, ScheduledAt: origin.Add(offset), Status: OpenLoopNotOffered,
		}
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	cutoff := origin.Add(cfg.ArrivalOffsets[len(cfg.ArrivalOffsets)-1] + cfg.DrainTimeout)
	parentDeadline, hasParentDeadline := ctx.Deadline()
	if hasParentDeadline && !origin.Before(parentDeadline) {
		return report, context.DeadlineExceeded
	}
	if hasParentDeadline && parentDeadline.Before(cutoff) {
		cutoff = parentDeadline
	}
	runCtx, cancel := context.WithDeadline(ctx, cutoff)
	defer cancel()
	s := &openLoopRun{report: report, cutoff: cutoff, wake: make(chan struct{}, 1)}
	next := 0
	for {
		now := time.Now()
		if runCtx.Err() != nil || !now.Before(cutoff) {
			break
		}
		if next < len(cfg.ArrivalOffsets) && !now.Before(origin.Add(cfg.ArrivalOffsets[next])) {
			if !s.offer(runCtx, cfg, next) {
				break
			}
			next++
			continue
		}
		s.mu.Lock()
		complete := next == len(cfg.ArrivalOffsets) && s.active == 0
		s.mu.Unlock()
		if complete {
			break
		}
		until := cutoff
		if next < len(cfg.ArrivalOffsets) {
			until = origin.Add(cfg.ArrivalOffsets[next])
			if until.After(cutoff) {
				until = cutoff
			}
		}
		timer := time.NewTimer(time.Until(until))
		select {
		case <-timer.C:
		case <-s.wake:
		case <-runCtx.Done():
		}
		timer.Stop()
	}
	return report, s.finish(ctx)
}

type openLoopRun struct {
	mu     sync.Mutex
	report *OpenLoopReport
	cutoff time.Time
	active int
	wake   chan struct{}
}

func (s *openLoopRun) offer(ctx context.Context, cfg OpenLoopScenario, index int) bool {
	s.mu.Lock()
	now := time.Now()
	if ctx.Err() != nil || !now.Before(s.cutoff) {
		s.mu.Unlock()
		return false
	}
	r := &s.report.Results[index]
	deadline := r.ScheduledAt.Add(cfg.Timeout)
	if !now.Before(deadline) {
		r.Status, r.Error = OpenLoopTimedOut, "arrival deadline exceeded before start"
		s.mu.Unlock()
		return true
	}
	if s.active >= cfg.MaxInFlight {
		r.Status, r.Error = OpenLoopRejected, "generator in-flight limit reached"
		s.mu.Unlock()
		return true
	}
	s.active++
	r.StartedAt, r.Status = now, OpenLoopUnfinished
	s.mu.Unlock()
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	go s.invoke(callCtx, cancel, cfg.Run, index, deadline)
	return true
}

func (s *openLoopRun) invoke(ctx context.Context, cancel context.CancelFunc, run func(context.Context, int) error, index int, deadline time.Time) {
	defer cancel()
	err := ctx.Err()
	if err == nil {
		err = run(ctx, index+1)
	}
	// Error and errors.Is may invoke user code. Keep the wrapper's slot while
	// inspecting it, but never let that work block the bookkeeping lock.
	status, detail := openLoopError(err)
	s.mu.Lock()
	s.active--
	now := time.Now()
	if s.report != nil && now.Before(s.cutoff) {
		r := &s.report.Results[index]
		if !now.Before(deadline) || ctx.Err() == context.DeadlineExceeded {
			status, detail = OpenLoopTimedOut, "arrival deadline exceeded"
		} else if ctx.Err() != nil {
			status, detail = OpenLoopCanceled, "callback context canceled"
		}
		r.Status, r.Error = status, detail
		r.FinishedAt, r.Latency = now, now.Sub(r.ScheduledAt)
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func openLoopError(err error) (OpenLoopStatus, string) {
	if err == nil {
		return OpenLoopSucceeded, ""
	}
	status := OpenLoopFailed
	if errors.Is(err, context.DeadlineExceeded) {
		status = OpenLoopTimedOut
	} else if errors.Is(err, context.Canceled) {
		status = OpenLoopCanceled
	}
	return status, err.Error()
}

func (s *openLoopRun) finish(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Timestamp the endpoint under the same lock as completions so an accepted
	// completion cannot appear later than the report's observation window.
	end := time.Now()
	if end.After(s.cutoff) {
		end = s.cutoff
	}
	err := ctx.Err()
	if deadline, ok := ctx.Deadline(); err == nil && ok && !end.Before(deadline) {
		err = context.DeadlineExceeded
	}
	s.report.EndTime = end
	for i := range s.report.Results {
		r := &s.report.Results[i]
		if r.Status == OpenLoopNotOffered && !r.ScheduledAt.After(end) {
			if err != nil {
				r.Status, r.Error = OpenLoopCanceled, "caller interrupted arrival before start"
			} else {
				r.Status, r.Error = OpenLoopTimedOut, "observation deadline exceeded before start"
			}
		}
		if r.Status == OpenLoopUnfinished {
			r.Error = "wrapper completion not recorded before observation ended"
		}
		if r.Status != OpenLoopNotOffered {
			s.report.Offered++
		}
	}
	// Transfer exclusive ownership to the caller. Late wrappers only release
	// their slots and cannot retain or change the published result slice.
	s.report = nil
	return err
}
