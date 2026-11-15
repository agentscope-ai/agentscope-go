package pipeline

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/runctx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// TeamRun owns participant events and validated host input. Use Done/Wait for
// team completion: individual ReplyEnd events belong to participants. Cancel
// does not roll back side effects. A tool ignoring cancellation delays Wait.
type TeamRun struct {
	ctx      context.Context
	cancel   context.CancelFunc
	events   chan event.Event
	done     chan struct{}
	workers  map[string]*agent.UnifiedAgent
	mu       sync.Mutex
	pending  *teamPending
	finished bool
	err      error
}

func newTeamRun(ctx context.Context, cancel context.CancelFunc, workers map[string]*agent.UnifiedAgent) *TeamRun {
	return &TeamRun{ctx: ctx, cancel: cancel, events: make(chan event.Event, 32), done: make(chan struct{}), workers: workers}
}

// Events returns the original participant events. Drain it or call Cancel;
// abandoning a normal run's events applies backpressure and can block Wait.
func (r *TeamRun) Events() <-chan event.Event {
	if r == nil {
		return nil
	}
	return r.events
}

// Done closes after all accepted participant cores have actually exited.
func (r *TeamRun) Done() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.done
}

// Cancel stops further admission and cancels active participants. Wait joins
// accepted cores; cancellation alone is not proof that their work has stopped.
func (r *TeamRun) Cancel() {
	if r != nil && r.cancel != nil {
		r.cancel()
	}
}

// Wait joins the run. Participant failures become assignment results; a leader
// failure or caller cancellation returns an error. No detached tool-owned work
// is tracked, and no deadline can release ownership of a still-running core.
func (r *TeamRun) Wait() error {
	if r == nil || r.done == nil {
		return ErrTeamClosed
	}
	<-r.done
	return r.err
}

func (r *TeamRun) finish(err error) {
	if r.ctx.Err() != nil {
		err = r.ctx.Err()
	}
	r.mu.Lock()
	r.err = err
	r.finished = true
	r.pending = nil
	r.mu.Unlock()
	r.cancel()
	close(r.events)
}

func (r *TeamRun) emit(ctx context.Context, ev event.Event) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case r.events <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

type teamOutcome struct {
	text   string
	reason types.ReplyFinishedReason
	err    error
}

func (r *TeamRun) participant(parent context.Context, a *agent.UnifiedAgent, input string, leader bool) (out teamOutcome) {
	if err := parent.Err(); err != nil {
		out.err = err
		return out
	}
	// Preserve an outer application/leader scope when replacing the context
	// value with this invocation's scope. A follow-up always gets a new scope.
	release, accepted := runctx.Start(parent)
	if !accepted {
		out.err = ErrTeamClosed
		return out
	}
	ctx, cancel := context.WithCancel(parent)
	ctx, scope := runctx.New(ctx)
	defer func() { cancel(); scope.Close(); release() }()
	stream, err := a.ReplyStream(ctx, input)
	if err != nil {
		out.err = err
		return out
	}
	var replyID string
	var end *event.ReplyEndEvent
	var text strings.Builder
	modelRound, partial := false, false
	for ev := range stream {
		if ctx.Err() != nil {
			break
		}
		switch e := ev.(type) {
		case event.ReplyStartEvent:
			replyID = e.ReplyID
			end = nil
			text.Reset()
			modelRound = false
			partial = false
		case event.ModelCallStartEvent:
			if e.ReplyID == replyID {
				text.Reset()
				modelRound = true
				partial = false
			}
		case event.TextBlockStartEvent:
			if e.ReplyID == replyID && modelRound && text.Len() > 0 {
				text.WriteByte('\n')
			}
		case event.TextBlockDeltaEvent:
			if e.ReplyID == replyID && modelRound {
				text.WriteString(e.Delta)
			}
		case event.ReplyEndEvent:
			if e.ReplyID == replyID {
				cp := e
				end = &cp
			}
		case event.CustomEvent:
			if e.ReplyID == replyID && e.Name == "model_partial_response" {
				partial = true
			}
		case event.RequireExternalExecutionEvent:
			if e.ReplyID != replyID {
				out.err = fmt.Errorf("%w: external request without active reply", ErrTeamInput)
				return out
			}
			if leader && len(e.ToolCalls) == 1 && e.ToolCalls[0].Name == TeamAssignToolName {
				result := r.assign(ctx, &e.ToolCalls[0])
				if ctx.Err() != nil {
					out.err = ctx.Err()
					return out
				}
				a.SubmitExternalResult(&event.ExternalExecutionResultEvent{ReplyID: replyID, ExecutionResults: []message.ToolResultBlock{result}})
				continue
			}
			if err := r.hostResponse(ctx, a, ev); err != nil {
				out.err = err
				return out
			}
			continue
		case event.RequireUserConfirmEvent:
			if e.ReplyID != replyID {
				out.err = fmt.Errorf("%w: confirmation without active reply", ErrTeamInput)
				return out
			}
			if err := r.hostResponse(ctx, a, ev); err != nil {
				out.err = err
				return out
			}
			continue
		}
		if !r.emit(ctx, ev) {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		out.err = err
		return out
	}
	if replyID == "" || end == nil {
		out.err = fmt.Errorf("pipeline: %s stream ended without a matching terminal event", a.Name())
		return out
	}
	out.reason = end.FinishedReason
	// A handed-off end is a normal end: for the leader the foreground moved to
	// another participant, which ends the run without error; members never
	// receive a transfer tool, so a member handoff is treated as completed.
	completed := end.FinishedReason == types.ReplyCompleted || end.FinishedReason == types.ReplyHandedOff
	if !completed || partial {
		out.err = fmt.Errorf("pipeline: %s finished as %s", a.Name(), end.FinishedReason)
		if completed {
			out.err = fmt.Errorf("pipeline: %s final model round is incomplete", a.Name())
		}
		if end.Error != nil {
			out.err = fmt.Errorf("%w: %s", out.err, end.Error.Message)
		}
		return out
	}
	out.text = text.String()
	if !leader && (!modelRound || strings.TrimSpace(out.text) == "") {
		out.err = ErrTeamOutput
	}
	return out
}

func (r *TeamRun) assign(ctx context.Context, call *message.ToolCallBlock) message.ToolResultBlock {
	result := message.ToolResultBlock{Type: "tool_result", ID: call.ID, Name: call.Name, State: message.ToolResultError}
	input, err := call.ParseInput()
	if err != nil {
		result.Output = "invalid TeamAssign input"
		return result
	}
	name, _ := input["member"].(string)
	prompt, _ := input["prompt"].(string)
	worker := r.workers[name]
	if worker == nil || strings.TrimSpace(prompt) == "" {
		result.Output = "unknown member or empty assignment prompt"
		return result
	}
	if ctx.Err() != nil {
		result.State = message.ToolResultInterrupted
		result.Output = "team canceled before assignment"
		return result
	}
	out := r.participant(ctx, worker, prompt, false)
	if out.err != nil {
		result.Output = out.err.Error()
		if out.reason == types.ReplyInterrupted || ctx.Err() != nil {
			result.State = message.ToolResultInterrupted
		}
		return result
	}
	result.State = message.ToolResultSuccess
	result.Output = out.text
	return result
}
