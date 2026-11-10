package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/runctx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// ErrGoalInteraction means Stage 1 cannot continue without a host response.
var ErrGoalInteraction = errors.New("pipeline: goal requires interactive confirmation or external execution")

// GoalAgentConfig bounds additional output-correction attempts, not work or
// provider calls. Zero means one initial output attempt and no corrections.
type GoalAgentConfig struct{ MaxOutputRetries int }

// GoalAgentFactory creates an independently configured verifier per check.
// The caller owns models/workspaces; returning a shared active agent is invalid.
type GoalAgentFactory func(context.Context) (*agent.UnifiedAgent, error)

type goalAgent struct {
	executor *agent.UnifiedAgent
	factory  GoalAgentFactory
	cfg      GoalAgentConfig
	busy     atomic.Bool
}

// NewGoalExecutor adapts an exclusively owned agent, preserving its history
// across attempts. Do not use that agent concurrently through another API.
func NewGoalExecutor(a *agent.UnifiedAgent, cfg GoalAgentConfig) (GoalExecutor, error) {
	if a == nil || cfg.MaxOutputRetries < 0 {
		return nil, fmt.Errorf("pipeline: agent required and MaxOutputRetries must be nonnegative")
	}
	return &goalAgent{executor: a, cfg: cfg}, nil
}

// NewGoalVerifier creates a verifier adapter with fresh conversation state for
// every check. Unlike Python's reset, the factory replaces all agent-local state.
func NewGoalVerifier(factory GoalAgentFactory, cfg GoalAgentConfig) (GoalVerifier, error) {
	if factory == nil || cfg.MaxOutputRetries < 0 {
		return nil, fmt.Errorf("pipeline: factory required and MaxOutputRetries must be nonnegative")
	}
	return &goalAgent{factory: factory, cfg: cfg}, nil
}

var goalReportSchema = json.RawMessage(`{"type":"object","properties":{"report":{"type":"string","minLength":1,"maxLength":65536}},"required":["report"],"additionalProperties":false}`)
var goalVerificationSchema = json.RawMessage(`{"type":"object","properties":{"result":{"type":"string","enum":["pass","fail","impossible"]},"message":{"type":"string","minLength":1,"maxLength":65536}},"required":["result","message"],"additionalProperties":false}`)

func (g *goalAgent) Execute(ctx context.Context, goal, feedback string) (ExecutionReport, error) {
	var out ExecutionReport
	if !g.busy.CompareAndSwap(false, true) {
		return out, ErrGoalBusy
	}
	defer g.busy.Store(false)
	if g.executor == nil {
		return out, fmt.Errorf("pipeline: adapter has no executor")
	}
	prompt := fmt.Sprintf("Complete this goal and describe the concrete artifacts and how to check them.\nGoal:\n%s\nPrevious verification feedback:\n%s", goal, feedback)
	usage, err := g.run(ctx, g.executor, prompt, goalReportSchema, &out)
	out.Usage = usage.Usage
	out.UsageComplete = usage.UsageComplete
	return out, err
}
func (g *goalAgent) Verify(ctx context.Context, goal string, report ExecutionReport) (Verification, error) {
	var out Verification
	if !g.busy.CompareAndSwap(false, true) {
		return out, ErrGoalBusy
	}
	defer g.busy.Store(false)
	if ctx == nil {
		return out, fmt.Errorf("pipeline: nil context")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if g.factory == nil {
		return out, fmt.Errorf("pipeline: adapter has no verifier factory")
	}
	a, err := g.factory(ctx)
	if err != nil {
		return out, fmt.Errorf("pipeline: verifier factory: %w", err)
	}
	if a == nil {
		return out, fmt.Errorf("pipeline: verifier factory returned nil")
	}
	prompt := fmt.Sprintf("Independently check the actual artifacts against the goal. Use your tools to verify evidence. Return pass, fail, or impossible with specific findings.\nGoal:\n%s\nExecutor's report (claims to check):\n%s", goal, report.Report)
	usage, err := g.run(ctx, a, prompt, goalVerificationSchema, &out)
	out.Usage = usage.Usage
	out.UsageComplete = usage.UsageComplete
	return out, err
}

func (g *goalAgent) run(ctx context.Context, a *agent.UnifiedAgent, prompt string, schema json.RawMessage, out any) (agent.StructuredResponse, error) {
	total := agent.StructuredResponse{}
	if ctx == nil {
		return total, fmt.Errorf("pipeline: nil context")
	}
	if err := ctx.Err(); err != nil {
		return total, err
	}
	budgetCtx, err := middleware.WithReplyBudgetSnapshot(ctx, nil)
	if err != nil {
		return total, err
	}
	workUsage, err := runGoalWork(budgetCtx, a, prompt)
	total.Usage = workUsage
	// Legacy work events do not distinguish absent usage from known zero, nor
	// expose all retries inside Chat. Never label that total as complete.
	if err != nil {
		return total, err
	}
	var outputErr error
	for attempt := 0; attempt <= g.cfg.MaxOutputRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		r, callErr := a.GenerateStructuredResponseWithUsage(budgetCtx, schema)
		if r != nil {
			addGoalUsage(&total.Usage, r.Usage)
		}
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		if callErr != nil {
			if !errors.Is(callErr, agenterrors.ErrStructuredOutput) {
				return total, callErr
			}
			outputErr = callErr
			continue
		}
		if r == nil {
			return total, fmt.Errorf("pipeline: missing structured response")
		}
		outputErr = decodeGoalOutput(r.Output, out)
		if outputErr == nil {
			return total, nil
		}
	}
	return total, fmt.Errorf("%w: invalid goal output after %d attempts: %v", agenterrors.ErrStructuredOutput, g.cfg.MaxOutputRetries+1, outputErr)
}

func runGoalWork(ctx context.Context, a *agent.UnifiedAgent, prompt string) (*model.ChatUsage, error) {
	// Register the adapter with an outer App scope before creating its local
	// scope; replacing the context value must not lose the outer ownership.
	release, accepted := runctx.Start(ctx)
	if !accepted {
		return nil, fmt.Errorf("pipeline: parent run already ended")
	}
	defer release()
	workCtx, cancel := context.WithCancel(ctx)
	workCtx, scope := runctx.New(workCtx)
	ch, err := a.ReplyStream(workCtx, prompt)
	if err != nil {
		cancel()
		scope.Close()
		return nil, err
	}
	var replyID string
	var end *event.ReplyEndEvent
	var usage *model.ChatUsage
	interaction := false
	partial := false
	for evt := range ch {
		switch e := evt.(type) {
		case event.ReplyStartEvent:
			replyID = e.ReplyID
			end = nil
		case event.ReplyEndEvent:
			if e.ReplyID == replyID {
				cp := e
				end = &cp
			}
		case event.ModelCallEndEvent:
			if e.InputTokens != 0 || e.OutputTokens != 0 || e.CacheCreationTokens != 0 || e.CacheReadTokens != 0 {
				addGoalUsage(&usage, &model.ChatUsage{InputTokens: e.InputTokens, OutputTokens: e.OutputTokens, CacheCreationInputTokens: e.CacheCreationTokens, CacheInputTokens: e.CacheReadTokens})
			}
		case event.RequireUserConfirmEvent, event.RequireExternalExecutionEvent:
			interaction = true
			cancel()
		case event.CustomEvent:
			if e.Name == "model_partial_response" {
				partial = true
			}
		}
	}
	cancel()
	scope.Close()
	if ctx.Err() != nil {
		return usage, ctx.Err()
	}
	if interaction {
		return usage, ErrGoalInteraction
	}
	if end == nil || replyID == "" {
		return usage, fmt.Errorf("pipeline: work stream ended without a matching terminal event")
	}
	if end.FinishedReason != types.ReplyCompleted || partial {
		if end.Error != nil {
			return usage, fmt.Errorf("pipeline: agent work failed: %s", end.Error.Message)
		}
		// A handed-off end stopped the work after a tool step: the goal is
		// not complete here, so it is not reported or verified as if it were.
		if end.FinishedReason == types.ReplyHandedOff {
			return usage, errors.New("pipeline: agent work was handed off before completion")
		}
		return usage, fmt.Errorf("pipeline: agent work did not complete: %s", end.FinishedReason)
	}
	return usage, nil
}

// Goal's schemas have one or two string fields. Validate these explicitly,
// including duplicate/unknown keys, rather than relying on provider compliance.
func decodeGoalOutput(raw json.RawMessage, out any) error {
	if len(raw) > 128*1024 {
		return fmt.Errorf("goal output exceeds 128 KiB")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return fmt.Errorf("goal output must be an object")
	}
	seen := map[string]bool{}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err = dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok || seen[key] {
			return fmt.Errorf("duplicate or invalid output field")
		}
		seen[key] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
		fields[key] = value
	}
	if _, err = dec.Token(); err != nil {
		return err
	}
	if _, err = dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing output")
	}
	read := func(key string) (string, error) {
		var s string
		if err := json.Unmarshal(fields[key], &s); err != nil {
			return "", err
		}
		if strings.TrimSpace(s) == "" || len(s) > 65536 {
			return "", fmt.Errorf("invalid %s", key)
		}
		return s, nil
	}
	switch v := out.(type) {
	case *ExecutionReport:
		if len(fields) != 1 {
			return fmt.Errorf("expected report only")
		}
		s, err := read("report")
		if err != nil {
			return err
		}
		v.Report = s
	case *Verification:
		if len(fields) != 2 {
			return fmt.Errorf("expected result and message")
		}
		s, err := read("result")
		if err != nil {
			return err
		}
		if Verdict(s) != VerdictPass && Verdict(s) != VerdictFail && Verdict(s) != VerdictImpossible {
			return fmt.Errorf("unknown verdict")
		}
		m, err := read("message")
		if err != nil {
			return err
		}
		v.Result = Verdict(s)
		v.Message = m
	default:
		return fmt.Errorf("unsupported goal output")
	}
	return nil
}
