package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/logging"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
)

// Checkpointing and crash-recovery (HARNESS_DESIGN F1).
//
// Checkpoints are written through the configured StateSaver at safe points,
// among them after each executed tool batch and before a reply ends waiting
// (StateWait). A reply blocked in process on a user confirmation or an
// external result saves nothing while it waits; a host that saves the state
// then records its calls as asking or submitted. The persisted AgentState
// carries the full resumable surface: context, summary, reply ID, iteration
// counter, permission context, activated tool groups, task state,
// middleware state and the read cache.
//
// Crash semantics (documented contract):
//   - A crash MID-BATCH: ResumeReplyStream re-executes the whole batch; a
//     reply with new input closes its calls as interrupted instead. Tool side
//     effects are therefore NOT exactly-once across crashes; prefer
//     read-only/idempotent tools in crash-sensitive deployments.
//   - A crash while parked: ResumeReplyStream (WithReplyRecovery) continues
//     the parked calls under the checkpoint's reply ID. A call awaiting
//     approval goes through the current permission check again
//     (resetRecoveryApprovals), and a submitted call is requested again
//     (repromptAwaitingCalls). A reply with new input closes them instead
//     (interruptUnansweredCallsLocked).

// Checkpoint persists the current agent state via the configured StateSaver.
// It is safe to call without a saver (no-op). Errors are logged, never
// returned — persistence failures must not kill an in-flight reply.
func (a *UnifiedAgent) Checkpoint(ctx context.Context) {
	if a.stateSaver == nil {
		return
	}
	if err := a.SaveCheckpoint(ctx); err != nil {
		logging.Warn("checkpoint failed", "err", err)
	}
}

// SaveCheckpoint persists an owned state snapshot and returns every failure.
// Unlike legacy Checkpoint, a missing StateSaver is an error. For opt-in active
// recovery it includes synchronized counters; automatic safe points stop the
// reply if persistence fails. Successful saving does not make tools exactly-once.
func (a *UnifiedAgent) SaveCheckpoint(ctx context.Context) error {
	finishRetentionWrite := a.beginRetentionWrite()
	defer finishRetentionWrite()
	a.checkpointMu.Lock()
	defer a.checkpointMu.Unlock()
	if a.stateSaver == nil {
		return fmt.Errorf("agent: no StateSaver configured")
	}
	a.mu.Lock()
	st := *a.state
	// Sync permission state from the engine: session-learned rules and
	// user-granted working directories live there, while a.state.PermissionCtx
	// only reflects what a checkpoint restore placed in it. SnapshotContext
	// deep-copies under the engine lock; the lock order here is always
	// agent.mu -> engine.mu and the engine never calls back into the agent.
	if a.engine != nil {
		st.PermissionCtx = a.engine.SnapshotContext()
	}
	// Preserve legacy writes for readers that support only schema 1. Recovery
	// counters require schema 2 and thread state schema 3; neither may
	// silently disappear on downgrade, so their presence raises the version.
	run := a.activeRecovery
	if run != nil {
		st.ReplyRecovery = run.snapshot()
	}
	st.SchemaVersion = st.checkpointSchemaVersion()
	if st.ReplyRecovery != nil {
		if _, err := middleware.WithReplyBudgetSnapshot(ctx, &st.ReplyRecovery.Budgets); err != nil {
			a.mu.Unlock()
			return fmt.Errorf("agent: checkpoint budgets: %w", err)
		}
	}
	raw, err := json.Marshal(&st)
	a.mu.Unlock()
	if err != nil {
		return fmt.Errorf("agent: checkpoint marshal: %w", err)
	}
	var detached AgentState
	if err = json.Unmarshal(raw, &detached); err != nil {
		return fmt.Errorf("agent: checkpoint snapshot: %w", err)
	}
	// Keep a separate copy for the agent: a saver owns and may mutate its input.
	var own AgentState
	if err = json.Unmarshal(raw, &own); err != nil {
		return fmt.Errorf("agent: checkpoint snapshot: %w", err)
	}
	if err = a.stateSaver.SaveState(ctx, detached.SessionID, &detached); err != nil {
		return fmt.Errorf("agent: save checkpoint: %w", err)
	}
	a.mu.Lock()
	if a.activeRecovery == run {
		a.state.ReplyRecovery = own.ReplyRecovery
	}
	a.mu.Unlock()
	return nil
}

// ForkSession branches the current session: it snapshots the live state
// through the same serialization path as SaveCheckpoint and persists it under
// newSessionID, leaving the running session (and any existing checkpoint of
// it) untouched. The fork captures exactly what LoadCheckpoint + WithState
// restores — context, summary, plan, permission grants and caches — with
// reply-recovery counters cleared, so the fork starts at a clean reply
// boundary.
//
// Contract: call between replies. Forking mid-reply snapshots whatever
// partial context the loop has produced so far, which is well-defined but
// rarely what a caller wants. Returns an error when no StateSaver is
// configured or newSessionID is empty.
func (a *UnifiedAgent) ForkSession(ctx context.Context, newSessionID string) error {
	if newSessionID == "" {
		return fmt.Errorf("agent: fork session: empty session id")
	}
	a.checkpointMu.Lock()
	defer a.checkpointMu.Unlock()
	if a.stateSaver == nil {
		return fmt.Errorf("agent: no StateSaver configured")
	}
	a.mu.Lock()
	st := *a.state
	if a.engine != nil {
		st.PermissionCtx = a.engine.SnapshotContext()
	}
	a.mu.Unlock()
	st.SessionID = newSessionID
	st.ReplyID = ""
	st.ReplyRecovery = nil
	st.SchemaVersion = st.checkpointSchemaVersion() // 1, or 3 with thread state
	raw, err := json.Marshal(&st)
	if err != nil {
		return fmt.Errorf("agent: fork marshal: %w", err)
	}
	var detached AgentState
	if err = json.Unmarshal(raw, &detached); err != nil {
		return fmt.Errorf("agent: fork snapshot: %w", err)
	}
	if err = a.stateSaver.SaveState(ctx, newSessionID, &detached); err != nil {
		return fmt.Errorf("agent: save fork: %w", err)
	}
	return nil
}

// LoadCheckpoint retrieves a persisted state for a session, ready to pass to
// NewUnifiedAgent via WithState. Returns an error when the saver has no
// state for the session.
func LoadCheckpoint(ctx context.Context, saver StateSaver, sessionID string) (*AgentState, error) {
	if saver == nil {
		return nil, fmt.Errorf("agent: nil StateSaver")
	}
	st, err := saver.LoadState(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("agent: load checkpoint for %s: %w", sessionID, err)
	}
	if st == nil {
		return nil, fmt.Errorf("agent: no checkpoint for session %s", sessionID)
	}
	if st.SchemaVersion > StateSchemaVersion {
		return nil, fmt.Errorf("agent: checkpoint schema version %d is newer than supported %d",
			st.SchemaVersion, StateSchemaVersion)
	}
	return st, nil
}

// submittedCalls scans backwards for the most recent assistant message of
// this agent and returns its submitted calls without a result (they need
// external results). The backwards scan skips messages of others; a reply
// with new input has already interrupted such calls
// (interruptUnansweredCallsLocked).
func (a *UnifiedAgent) submittedCalls() []message.ToolCallBlock {
	var submitted []message.ToolCallBlock
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.state.Context) - 1; i >= 0; i-- {
		msg := a.state.Context[i]
		if msg == nil || msg.Role != message.RoleAssistant || msg.Name != a.name {
			continue
		}
		finished := map[string]bool{}
		for _, b := range msg.GetContentBlocks(message.ContentBlockToolResult) {
			if tr, ok := b.(message.ToolResultBlock); ok {
				finished[tr.ID] = true
			}
		}
		for _, b := range msg.GetContentBlocks(message.ContentBlockToolCall) {
			tc, ok := b.(message.ToolCallBlock)
			if !ok || finished[tc.ID] {
				continue
			}
			if a.retentionOwned != nil {
				v, err := a.retentionCopyLocked(context.Background(), tc)
				if err != nil {
					a.retentionOwned.invalid = true
				} else {
					tc = v.(message.ToolCallBlock)
				}
			}
			if tc.State == message.ToolCallSubmitted {
				submitted = append(submitted, tc)
			}
		}
		return submitted // only the most recent assistant message matters
	}
	return nil
}

// Results for the calls a reply with new input closes
// (interruptUnansweredCallsLocked).
const (
	interruptedApprovalResult = "Tool call did not run: new input arrived while it waited for the user's approval, " +
		"so the request was closed unanswered. Nobody refused it; call it again if it is still needed."
	interruptedExternalResult = "Tool call interrupted: new input arrived before the host returned its result, " +
		"so its outcome is unknown, and a result the host returns now is not recorded. " +
		"If it could have changed something, check whether it did before calling it again."
	interruptedUnknownResult = "Tool call interrupted: new input arrived before its result was recorded, " +
		"so its outcome is unknown. If it could have changed something, check whether it did before calling it again."
)

// ToolCallsInterruptedEvent names the CustomEvent a reply with new input
// emits, before its first model call, when it closes tool calls an earlier
// reply left without a result. Its value holds "calls": one map per closed
// call with "tool_call_id", "name", "input", "state" (the call's state
// before it was closed) and "result" (the text recorded as its result). The
// results are stored in the calls' own messages, not in this reply's.
const ToolCallsInterruptedEvent = "tool_calls_interrupted"

// interruptedCall is a call closed by interruptUnansweredCallsLocked, its
// state before, and the text of its result.
type interruptedCall struct {
	call  message.ToolCallBlock
	prior message.ToolCallState
	text  string
}

// interruptUnansweredCallsLocked closes this agent's tool calls that have no
// result when a reply starts with new input: nothing is executed and nothing
// is asked again. Such calls come from a checkpoint saved while a reply waited
// for an approval or an external result, or before a step's results were
// recorded; a reply canceled in process records a result for each call. A
// call that was waiting for the user's approval did not run. Any other may
// have: a host may have run a submitted external call, or may still be
// running it, and its result can no longer be recorded; a pending or
// allowed call was cut off mid-step. Both have an unknown outcome. A call
// whose state is already finished is left as it is.
// Each result is recorded in its call's message, so the input follows the
// pair: providers reject a message between a call and its result (Anthropic
// documents it), and a result after the input also misled the compression
// split (#207).
// ResumeReplyStream, which appends no input, continues such calls instead.
// A result, in any message, answers one call: the newest earlier call of its
// ID still without one. That call is usually in the result's message; hosts
// may record results in messages of their own (openaiserver does), an earlier
// version recorded a call asked again after new input in a later message,
// and call IDs can repeat across replies. The caller holds a.mu and has
// checked that no other reply core of this agent is running.
func (a *UnifiedAgent) interruptUnansweredCallsLocked() []interruptedCall {
	type callPos struct {
		msg *message.Msg
		idx int
	}
	var calls []callPos
	var answered []bool
	open := map[string][]int{} // tool-call ID -> unanswered entries of calls, oldest first
	for _, msg := range a.state.Context {
		if msg == nil {
			continue
		}
		for i, b := range msg.Content {
			switch blk := b.(type) {
			case message.ToolCallBlock:
				open[blk.ID] = append(open[blk.ID], len(calls))
				calls = append(calls, callPos{msg: msg, idx: i})
				answered = append(answered, false)
			case message.ToolResultBlock:
				if q := open[blk.ID]; len(q) > 0 {
					answered[q[len(q)-1]] = true
					open[blk.ID] = q[:len(q)-1]
				}
			}
		}
	}

	var closed []interruptedCall
	updated := map[*message.Msg][]message.ContentBlock{}
	for k, pos := range calls {
		if answered[k] || pos.msg.Role != message.RoleAssistant || pos.msg.Name != a.name {
			continue
		}
		content, ok := updated[pos.msg]
		if !ok {
			content = pos.msg.Content
		}
		tc := content[pos.idx].(message.ToolCallBlock)
		if tc.State == message.ToolCallFinished {
			continue
		}
		if !ok {
			// Copy before writing: a snapshot taken for background
			// precompression may still read the stored slice.
			content = append([]message.ContentBlock(nil), content...)
		}
		prior := tc.State
		text := interruptedUnknownResult
		switch prior {
		case message.ToolCallAsking:
			text = interruptedApprovalResult
		case message.ToolCallSubmitted:
			text = interruptedExternalResult
		}
		tc.State = message.ToolCallFinished
		content[pos.idx] = tc
		content = append(content, message.ToolResultBlock{
			Type: "tool_result", ID: tc.ID, Name: tc.Name, Output: text, State: message.ToolResultInterrupted,
		})
		updated[pos.msg] = content
		closed = append(closed, interruptedCall{call: tc, prior: prior, text: text})
	}
	for msg, content := range updated {
		msg.Content = content
	}
	if len(closed) > 0 {
		a.invalidateRetentionLocked()
	}
	return closed
}

// dropQueuedAnswers empties a handshake channel. A reply with new input calls
// it after closing calls, while no other core runs and before it asks
// anything: a value still queued answers an earlier request, possibly one of
// the closed calls, and must not answer a later call that reuses its ID.
func dropQueuedAnswers[T any](ch chan T) {
	if ch == nil {
		return
	}
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// emitInterruptedCalls reports the calls interruptUnansweredCallsLocked
// closed with one ToolCallsInterruptedEvent. No tool events are sent: the
// calls belong to an earlier reply and did not run in this one.
func emitInterruptedCalls(ctx context.Context, ch chan<- event.Event, replyID string, closed []interruptedCall) {
	if len(closed) == 0 {
		return
	}
	calls := make([]map[string]any, 0, len(closed))
	for i := range closed {
		c := &closed[i]
		calls = append(calls, map[string]any{
			"tool_call_id": c.call.ID,
			"name":         c.call.Name,
			"input":        c.call.Input,
			"state":        string(c.prior),
			"result":       c.text,
		})
	}
	emit(ctx, ch, event.NewCustomEvent(replyID, ToolCallsInterruptedEvent, map[string]any{"calls": calls}))
}

// repromptAwaitingCalls re-drives the external handshakes of submitted calls
// after a resume: it re-requests their execution and records the outcomes,
// so a reply continued with ResumeReplyStream picks up where it parked
// instead of ending silently at StateWait. Calls awaiting approval are not
// asked about again: ResumeReplyStream returns them to pending for the
// current permission check (resetRecoveryApprovals), and a reply with new
// input closes them first (interruptUnansweredCallsLocked). Only while
// another reply core of this agent still runs, and may still be waiting
// for that approval, does such a call get here; it is left to that core and
// this reply ends at StateWait. Returns true if any awaiting call was
// handled (the react loop should re-evaluate).
//
// The loop hooks bracket each call as on the StateAct path: BeforeToolExec
// precedes the re-emitted confirmation (when the current policy asks) or
// execution request and AfterToolExec follows the recorded outcome, a denial
// included. They receive StateAct and the current iteration; the reported
// state stays StateWait, so no transition to StateAct is announced.
func (a *UnifiedAgent) repromptAwaitingCalls(ctx context.Context, ch chan<- event.Event, replyID string,
	actingHandler middleware.ActingHandler, hooks *loop.HookRunner, iter int) bool {
	const state = protocol.StateAct
	submitted := a.submittedCalls()
	handled := false
	for i := range submitted {
		tc := submitted[i]
		// A restored call must honor the current toolkit, permissions and
		// validators. Reuse normal recording and event assembly as well.
		hooks.BeforeToolExec(state, iter, tc.Name)
		a.executeAndRecord(ctx, ch, replyID, &tc, actingHandler, false)
		hooks.AfterToolExec(state, iter, tc.Name, nil)
		handled = true
	}
	return handled
}
