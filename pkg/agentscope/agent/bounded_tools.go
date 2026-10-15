package agent

import (
	"context"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
)

// WithToolConcurrencyLimit opts into experimental bounded tool dispatch for
// UnifiedAgent replies. Applying a nonpositive width panics during construction.
// Without this option the existing unbounded batch dispatch is unchanged.
//
// A concurrent batch uses at most width framework workers. Queue selection and
// result publication follow call order; invocation/completion order between
// workers is not guaranteed. Unsafe, confirmation and external-tool barriers
// remain sequential. Width one retains concurrent-batch publication timing.
//
// Cancellation stops further admission in the tool step and records never-admitted
// calls as interrupted. Admitted calls retain their actual outcomes and own their
// slots through return, even if the outward stream has already closed. Checkpoint
// failure/replay semantics are unchanged. This does not bound total batch storage,
// tool-owned goroutines, other replies/agents or deployment-wide occupancy, and
// does not change the sequential loop bridge.
func WithToolConcurrencyLimit(width int) AgentOption {
	return func(a *UnifiedAgent) {
		if width <= 0 {
			panic("agent: WithToolConcurrencyLimit requires a positive width")
		}
		a.toolConcurrencyLimit = width
	}
}

const undispatchedToolResult = "Tool call was not dispatched because the reply context ended"

// Both fresh and restored tool steps use this path. In particular, cancellation
// must stop later sequential/interactive batches, not just the current pool.
func (a *UnifiedAgent) executeBoundedToolStep(
	ctx context.Context, ch chan<- event.Event, replyID string,
	calls []message.ToolCallBlock, acting middleware.ActingHandler,
	hooks *loop.HookRunner, state protocol.LoopState, iter int,
) {
	if ctx.Err() != nil {
		a.recordUndispatchedCalls(ctx, ch, replyID, calls)
		return
	}
	for _, batch := range batchToolCalls(calls, a.toolkit, a.wouldRequireHITL) {
		if ctx.Err() != nil {
			a.recordUndispatchedCalls(ctx, ch, replyID, batch.calls)
			continue
		}
		if batch.concurrent && len(batch.calls) > 1 {
			a.executeBoundedBatch(ctx, ch, replyID, batch.calls, acting, hooks, state, iter)
			continue
		}
		for _, tc := range batch.calls {
			if ctx.Err() != nil {
				a.recordUndispatchedCalls(ctx, ch, replyID, []message.ToolCallBlock{tc})
				continue
			}
			hooks.BeforeToolExec(state, iter, tc.Name)
			a.executeAndRecord(ctx, ch, replyID, &tc, acting)
			hooks.AfterToolExec(state, iter, tc.Name, nil)
		}
	}
}

type boundedToolJob struct {
	index int
	start chan struct{}
}

func (a *UnifiedAgent) executeBoundedBatch(
	ctx context.Context, ch chan<- event.Event, replyID string,
	calls []message.ToolCallBlock, acting middleware.ActingHandler,
	hooks *loop.HookRunner, state protocol.LoopState, iter int,
) {
	results := undispatchedResults(len(calls))
	jobs := make(chan boundedToolJob)
	var workers sync.WaitGroup
	for range min(a.toolConcurrencyLimit, len(calls)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				// Handoff admits the call; the gate orders the serial Before
				// callback before execution without running hooks concurrently.
				<-job.start
				tc := calls[job.index]
				outcome := a.executeToolCallWithPermission(ctx, nil, replyID, &tc, acting)
				results[job.index] = toolResult{index: job.index, state: outcome.state, text: outcome.text, extra: outcome.extra}
			}
		}()
	}
	admitted := 0
admission:
	for i := range calls {
		if ctx.Err() != nil {
			break
		}
		job := boundedToolJob{index: i, start: make(chan struct{})}
		select {
		case jobs <- job:
			admitted++
			hooks.BeforeToolExec(state, iter, calls[i].Name)
			// A handed-off job is already admitted: cancellation must not
			// abandon its gate or discard its eventual actual outcome.
			close(job.start)
		case <-ctx.Done():
			break admission
		}
	}
	close(jobs)
	workers.Wait() // actual cleanup, not outward stream closure, releases slots
	a.recordConcurrentResults(ctx, ch, replyID, calls, results)
	for _, tc := range calls[:admitted] {
		hooks.AfterToolExec(state, iter, tc.Name, nil)
	}
}

func undispatchedResults(count int) []toolResult {
	results := make([]toolResult, count)
	for i := range results {
		results[i] = toolResult{index: i, state: message.ToolResultInterrupted, text: undispatchedToolResult}
	}
	return results
}

func (a *UnifiedAgent) recordUndispatchedCalls(ctx context.Context, ch chan<- event.Event, replyID string, calls []message.ToolCallBlock) {
	a.recordConcurrentResults(ctx, ch, replyID, calls, undispatchedResults(len(calls)))
}
