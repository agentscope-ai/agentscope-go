package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/logging"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
)

// Checkpointing and crash-recovery (HARNESS_DESIGN F1).
//
// Checkpoints are written through the configured StateSaver at safe points:
// after each executed tool batch and when the reply parks awaiting user
// confirmation or external execution results. The persisted AgentState
// carries the full resumable surface: context, summary, reply ID, iteration
// counter, permission context, activated tool groups, task state,
// middleware state and the read cache.
//
// Crash semantics (documented contract):
//   - A crash MID-BATCH resumes by re-executing the whole batch. Tool side
//     effects are therefore NOT exactly-once across crashes; prefer
//     read-only/idempotent tools in crash-sensitive deployments.
//   - Crashes while parked are lossless: on resume the agent RE-EMITS the
//     RequireUserConfirmEvent / RequireExternalExecutionEvent for the
//     pending calls and waits again (see repromptAwaitingCalls).

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
	a.checkpointMu.Lock()
	defer a.checkpointMu.Unlock()
	if a.stateSaver == nil {
		return fmt.Errorf("agent: no StateSaver configured")
	}
	a.mu.Lock()
	st := *a.state
	// Preserve legacy writes for readers that support only schema 1. Recovery
	// counters require schema 2 and must never silently disappear on downgrade.
	st.SchemaVersion = 1
	run := a.activeRecovery
	if run != nil {
		st.ReplyRecovery = run.snapshot()
	}
	if st.ReplyRecovery != nil {
		st.SchemaVersion = StateSchemaVersion
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

// awaitingCalls scans backwards for the most recent assistant message that
// still has unfinished tool calls and splits them into asking (needs user
// confirmation) and submitted (needs external results) buckets. The
// backwards scan covers the resume case, where a fresh user input follows
// the restored assistant message (HARNESS_DESIGN F1).
func (a *UnifiedAgent) awaitingCalls() (asking, submitted []message.ToolCallBlock) {
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
			switch tc.State {
			case message.ToolCallAsking:
				asking = append(asking, tc)
			case message.ToolCallSubmitted:
				submitted = append(submitted, tc)
			}
		}
		return asking, submitted // only the most recent assistant message matters
	}
	return nil, nil
}

// repromptAwaitingCalls re-drives HITL/external handshakes after a resume:
// it re-emits the confirmation/execution events for pending calls and
// records the outcomes, so a conversation restored via WithState continues
// where it parked instead of ending silently at StateWait. Returns true if
// any awaiting call was handled (the react loop should re-evaluate).
func (a *UnifiedAgent) repromptAwaitingCalls(ctx context.Context, ch chan<- event.Event, replyID string, actingHandler middleware.ActingHandler) bool {
	asking, submitted := a.awaitingCalls()
	handled := false

	for i := range asking {
		tc := asking[i]
		emit(ctx, ch, event.NewRequireUserConfirmEvent(replyID, []message.ToolCallBlock{tc}))
		confirmed, resultTC := a.waitForConfirmation(ctx, tc.ID)
		if !confirmed {
			a.saveToContext([]message.ContentBlock{message.ToolResultBlock{
				Type:   "tool_result",
				ID:     tc.ID,
				Name:   tc.Name,
				Output: "Permission denied by user",
				State:  message.ToolResultDenied,
			}}, nil)
			a.updateToolCallState(tc.ID, message.ToolCallFinished)
			handled = true
			continue
		}
		// Execute the confirmed call inline through the same path the
		// StateAct branch uses — otherwise the confirmed work would sit as
		// a dangling ALLOWED call in a non-terminal assistant message and
		// never run. Confirmers may echo the original asking-state block
		// back, so force the allowed state first.
		resultTC.State = message.ToolCallAllowed
		a.replaceToolCallInContext(tc.ID, &resultTC)
		a.executeAndRecord(ctx, ch, replyID, &resultTC, actingHandler)
		handled = true
	}

	for i := range submitted {
		tc := submitted[i]
		// A restored call must honor the current toolkit, permissions and
		// validators. Reuse normal recording and event assembly as well.
		a.executeAndRecord(ctx, ch, replyID, &tc, actingHandler)
		handled = true
	}

	return handled
}

// replaceToolCallInContext swaps the stored tool call (by ID) with the
// confirmed version returned by the user, preserving any edits made during
// confirmation.
func (a *UnifiedAgent) replaceToolCallInContext(callID string, replacement *message.ToolCallBlock) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.state.Context) - 1; i >= 0; i-- {
		msg := a.state.Context[i]
		if msg == nil || msg.Role != message.RoleAssistant || msg.Name != a.name {
			continue
		}
		for j, b := range msg.Content {
			if tc, ok := b.(message.ToolCallBlock); ok && tc.ID == callID {
				msg.Content[j] = *replacement
				return
			}
		}
	}
}
