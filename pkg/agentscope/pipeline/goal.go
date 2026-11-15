package pipeline

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// Verdict is a verifier's domain judgment, not a guarantee of correctness.
type Verdict string

const (
	VerdictPass       Verdict = "pass"
	VerdictFail       Verdict = "fail"
	VerdictImpossible Verdict = "impossible"
)

// ExecutionReport identifies artifacts and how to check the work.
// Usage covers only known model usage; false completeness is not zero cost.
type ExecutionReport struct {
	Report        string           `json:"report"`
	Usage         *model.ChatUsage `json:"-"`
	UsageComplete bool             `json:"-"`
}

// Verification contains an explicit judgment and supporting findings.
type Verification struct {
	Result        Verdict          `json:"result"`
	Message       string           `json:"message"`
	Usage         *model.ChatUsage `json:"-"`
	UsageComplete bool             `json:"-"`
}

// GoalExecutor performs one work attempt. Output correction must not repeat work.
type GoalExecutor interface {
	Execute(context.Context, string, string) (ExecutionReport, error)
}

// GoalVerifier checks the original goal against the reported artifacts.
type GoalVerifier interface {
	Verify(context.Context, string, ExecutionReport) (Verification, error)
}

// GoalConfig bounds work attempts. MaxIterations must be positive.
type GoalConfig struct{ MaxIterations int }

// GoalStatus distinguishes a settled judgment from operational failure.
type GoalStatus string

const (
	GoalPassed     GoalStatus = "passed"
	GoalImpossible GoalStatus = "impossible"
	GoalExhausted  GoalStatus = "exhausted"
	GoalError      GoalStatus = "error"
	GoalCanceled   GoalStatus = "canceled"
)

// GoalAttempt retains partial progress as well as completed verification.
type GoalAttempt struct {
	Report       ExecutionReport
	Verification *Verification
}

// GoalResult is an owned snapshot. A nil error does not imply GoalPassed.
type GoalResult struct {
	Status        GoalStatus
	Attempts      []GoalAttempt
	Usage         *model.ChatUsage
	UsageComplete bool
}

// ErrGoalBusy rejects overlapping runs on the same pipeline or agent adapter.
var ErrGoalBusy = errors.New("pipeline: goal run already active")

// GoalPipeline is an experimental bounded, sequential executor-verifier loop.
// It owns no workers, queue, persistence, or interactive resume protocol.
type GoalPipeline struct {
	executor      GoalExecutor
	verifier      GoalVerifier
	maxIterations int
	busy          atomic.Bool
}

// NewGoalPipeline constructs a loop without changing participant state.
// Participants must not be shared with other active calls.
func NewGoalPipeline(executor GoalExecutor, verifier GoalVerifier, cfg GoalConfig) (*GoalPipeline, error) {
	if nilGoalParticipant(executor) || nilGoalParticipant(verifier) {
		return nil, fmt.Errorf("pipeline: executor and verifier are required")
	}
	if cfg.MaxIterations <= 0 {
		return nil, fmt.Errorf("pipeline: MaxIterations must be positive")
	}
	return &GoalPipeline{executor: executor, verifier: verifier, maxIterations: cfg.MaxIterations}, nil
}
func nilGoalParticipant(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Map, reflect.Slice, reflect.Interface, reflect.Chan:
		return rv.IsNil()
	}
	return false
}

// Run performs up to MaxIterations work attempts. Passed, impossible and
// exhausted are nil-error outcomes. Errors retain prior and partial progress.
// Custom participants must honor ctx; calls are never automatically replayed.
func (p *GoalPipeline) Run(ctx context.Context, goal string) (GoalResult, error) {
	out := GoalResult{Status: GoalError, UsageComplete: true}
	if ctx == nil {
		return out, fmt.Errorf("pipeline: nil context")
	}
	if strings.TrimSpace(goal) == "" {
		return out, fmt.Errorf("pipeline: empty goal")
	}
	if !p.busy.CompareAndSwap(false, true) {
		return out, ErrGoalBusy
	}
	defer p.busy.Store(false)
	fail := func(err error) (GoalResult, error) {
		if ctx.Err() != nil {
			out.Status = GoalCanceled
			return out, ctx.Err()
		}
		return out, err
	}
	feedback := ""
	for i := 0; i < p.maxIterations; i++ {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		report, err := p.executor.Execute(ctx, goal, feedback)
		report.Usage = copyGoalUsage(report.Usage)
		out.Attempts = append(out.Attempts, GoalAttempt{Report: report})
		addGoalUsage(&out.Usage, report.Usage)
		out.UsageComplete = out.UsageComplete && report.UsageComplete
		if err != nil {
			return fail(fmt.Errorf("pipeline: execution attempt %d: %w", i+1, err))
		}
		if err = ctx.Err(); err != nil {
			return fail(err)
		}
		if strings.TrimSpace(report.Report) == "" {
			return fail(fmt.Errorf("pipeline: empty execution report"))
		}
		// Give custom verifiers their own copy, not the returned history's usage.
		input := report
		input.Usage = copyGoalUsage(report.Usage)
		v, err := p.verifier.Verify(ctx, goal, input)
		v.Usage = copyGoalUsage(v.Usage)
		out.Attempts[i].Verification = &v
		addGoalUsage(&out.Usage, v.Usage)
		out.UsageComplete = out.UsageComplete && v.UsageComplete
		if err != nil {
			return fail(fmt.Errorf("pipeline: verification attempt %d: %w", i+1, err))
		}
		if err = ctx.Err(); err != nil {
			return fail(err)
		}
		if strings.TrimSpace(v.Message) == "" {
			return fail(fmt.Errorf("pipeline: empty verification message"))
		}
		switch v.Result {
		case VerdictPass:
			out.Status = GoalPassed
			return out, nil
		case VerdictImpossible:
			out.Status = GoalImpossible
			return out, nil
		case VerdictFail:
			feedback = v.Message
		default:
			return fail(fmt.Errorf("pipeline: invalid verdict %q", v.Result))
		}
	}
	out.Status = GoalExhausted
	return out, nil
}
func copyGoalUsage(u *model.ChatUsage) *model.ChatUsage {
	if u == nil {
		return nil
	}
	cp := *u
	return &cp
}
func addGoalUsage(total **model.ChatUsage, u *model.ChatUsage) {
	if u == nil {
		return
	}
	if *total == nil {
		*total = &model.ChatUsage{}
	}
	if (*total).TotalInputTokens > 0 || u.TotalInputTokens > 0 {
		(*total).TotalInputTokens = (*total).EffectiveInputTokens() + u.EffectiveInputTokens()
	}
	(*total).Time += u.Time
	(*total).InputTokens += u.InputTokens
	(*total).OutputTokens += u.OutputTokens
	(*total).CacheCreationInputTokens += u.CacheCreationInputTokens
	(*total).CacheInputTokens += u.CacheInputTokens
}
