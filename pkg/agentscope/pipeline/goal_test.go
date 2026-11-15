package pipeline

import (
	"context"
	"errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"strings"
	"testing"
)

type goalExecutorFunc func(context.Context, string, string) (ExecutionReport, error)

func (f goalExecutorFunc) Execute(ctx context.Context, goal, feedback string) (ExecutionReport, error) {
	return f(ctx, goal, feedback)
}

type goalVerifierFunc func(context.Context, string, ExecutionReport) (Verification, error)

func (f goalVerifierFunc) Verify(ctx context.Context, goal string, r ExecutionReport) (Verification, error) {
	return f(ctx, goal, r)
}

func TestGoalPipelineFeedbackAndLastAttempt(t *testing.T) {
	calls := 0
	executor := goalExecutorFunc(func(_ context.Context, goal, feedback string) (ExecutionReport, error) {
		if goal != "pagination capped at 100" {
			t.Fatalf("goal changed: %q", goal)
		}
		if calls == 1 && feedback != "missing size cap" {
			t.Fatalf("feedback: %q", feedback)
		}
		calls++
		return ExecutionReport{Report: "handler.go and pagination tests"}, nil
	})
	verifier := goalVerifierFunc(func(_ context.Context, _ string, r ExecutionReport) (Verification, error) {
		if r.Report == "" {
			t.Fatal("report lost")
		}
		if calls == 1 {
			return Verification{Result: VerdictFail, Message: "missing size cap"}, nil
		}
		return Verification{Result: VerdictPass, Message: "acceptance checks passed"}, nil
	})
	p, err := NewGoalPipeline(executor, verifier, GoalConfig{MaxIterations: 2})
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Run(context.Background(), "pagination capped at 100")
	if err != nil || result.Status != GoalPassed || len(result.Attempts) != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Attempts[0].Verification.Result != VerdictFail {
		t.Fatal("history overwritten")
	}
}

func TestGoalPipelineOutcomesAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict Verdict
		want    GoalStatus
		wantErr bool
	}{
		{"impossible", VerdictImpossible, GoalImpossible, false},
		{"exhausted", VerdictFail, GoalExhausted, false},
		{"invalid", Verdict("maybe"), GoalError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 0
			e := goalExecutorFunc(func(context.Context, string, string) (ExecutionReport, error) {
				n++
				return ExecutionReport{Report: "artifact"}, nil
			})
			v := goalVerifierFunc(func(context.Context, string, ExecutionReport) (Verification, error) {
				return Verification{Result: tc.verdict, Message: "reason"}, nil
			})
			p, err := NewGoalPipeline(e, v, GoalConfig{MaxIterations: 2})
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.Run(context.Background(), "goal")
			if (err != nil) != tc.wantErr || got.Status != tc.want {
				t.Fatalf("%+v %v", got, err)
			}
			if tc.verdict == VerdictImpossible && n != 1 {
				t.Fatal("impossible retried")
			}
		})
	}
	cause := errors.New("disk failure")
	e := goalExecutorFunc(func(context.Context, string, string) (ExecutionReport, error) { return ExecutionReport{}, cause })
	v := goalVerifierFunc(func(context.Context, string, ExecutionReport) (Verification, error) {
		t.Fatal("verified failed work")
		return Verification{}, nil
	})
	p, _ := NewGoalPipeline(e, v, GoalConfig{MaxIterations: 2})
	got, err := p.Run(context.Background(), "goal")
	if !errors.Is(err, cause) || got.Status != GoalError {
		t.Fatalf("%+v %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err = p.Run(ctx, "goal")
	if !errors.Is(err, context.Canceled) || got.Status != GoalCanceled {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err = p.Run(context.Background(), " "); err == nil {
		t.Fatal("empty goal accepted")
	}
	if _, err = NewGoalPipeline(e, v, GoalConfig{}); err == nil {
		t.Fatal("zero budget accepted")
	}
	if _, err = NewGoalPipeline(nil, v, GoalConfig{MaxIterations: 1}); err == nil {
		t.Fatal("nil executor accepted")
	}
}

func TestGoalPipelineOverlapAndOutputValidation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	e := goalExecutorFunc(func(context.Context, string, string) (ExecutionReport, error) {
		close(started)
		<-release
		return ExecutionReport{Report: " "}, nil
	})
	v := goalVerifierFunc(func(context.Context, string, ExecutionReport) (Verification, error) {
		t.Fatal("empty report verified")
		return Verification{}, nil
	})
	p, _ := NewGoalPipeline(e, v, GoalConfig{MaxIterations: 1})
	done := make(chan error, 1)
	go func() { _, err := p.Run(context.Background(), "goal"); done <- err }()
	<-started
	if _, err := p.Run(context.Background(), "another"); !errors.Is(err, ErrGoalBusy) {
		t.Fatalf("overlap: %v", err)
	}
	close(release)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "report") {
		t.Fatalf("invalid report: %v", err)
	}
}

func TestGoalUsageOwnershipAndVerificationFailures(t *testing.T) {
	eu, vu := &model.ChatUsage{InputTokens: 2, TotalInputTokens: 5, Time: 0.5}, &model.ChatUsage{InputTokens: 3, OutputTokens: 3, Time: 0.25}
	e := goalExecutorFunc(func(context.Context, string, string) (ExecutionReport, error) {
		return ExecutionReport{Report: "artifact", Usage: eu, UsageComplete: true}, nil
	})
	v := goalVerifierFunc(func(_ context.Context, _ string, r ExecutionReport) (Verification, error) {
		r.Usage.InputTokens = 999
		return Verification{Result: VerdictPass, Message: "checked", Usage: vu, UsageComplete: true}, nil
	})
	p, _ := NewGoalPipeline(e, v, GoalConfig{MaxIterations: 1})
	out, err := p.Run(context.Background(), "goal")
	if err != nil || out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 3 || out.Usage.TotalInputTokens != 8 || out.Usage.Time != 0.75 || !out.UsageComplete {
		t.Fatalf("%+v %v", out, err)
	}
	eu.InputTokens = 77
	vu.OutputTokens = 88
	if out.Attempts[0].Report.Usage.InputTokens != 2 || out.Attempts[0].Verification.Usage.OutputTokens != 3 {
		t.Fatal("usage aliases participant")
	}
	for _, tc := range []struct {
		name    string
		cause   error
		message string
	}{{"error", errors.New("verification failed"), "reason"}, {"missing_message", nil, " "}} {
		t.Run(tc.name, func(t *testing.T) {
			check := goalVerifierFunc(func(context.Context, string, ExecutionReport) (Verification, error) {
				return Verification{Result: VerdictPass, Message: tc.message}, tc.cause
			})
			p, _ := NewGoalPipeline(e, check, GoalConfig{MaxIterations: 1})
			result, err := p.Run(context.Background(), "goal")
			if err == nil || result.Status != GoalError || len(result.Attempts) != 1 {
				t.Fatalf("%+v %v", result, err)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatal("error lost")
			}
		})
	}
	var nilFunc goalExecutorFunc
	if _, err = NewGoalPipeline(nilFunc, v, GoalConfig{MaxIterations: 1}); err == nil {
		t.Fatal("typed nil accepted")
	}
}
