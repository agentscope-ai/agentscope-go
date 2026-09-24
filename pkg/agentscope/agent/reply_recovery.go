package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// ReplyRecoveryState stores only owned, versioned state needed to preserve the
// built-in reply budgets across an explicit resume. Active=false is terminal and
// cannot be resumed. NextIteration preserves the remaining iteration allowance.
// This is not an exactly-once tool protocol or an in-flight cost reservation.
type ReplyRecoveryState struct {
	Version       int                            `json:"version"`
	ReplyID       string                         `json:"reply_id"`
	Active        bool                           `json:"active"`
	NextIteration int                            `json:"next_iteration"`
	Budgets       middleware.ReplyBudgetSnapshot `json:"budgets"`
}

// WithReplyRecovery enables typed budget checkpoints and fail-stop persistence.
// A StateSaver is required. Ordinary Reply/ReplyStream starts a fresh logical
// reply; ResumeReplyStream preserves its ID, budgets and iteration allowance.
// Concurrent replies on this agent are rejected while recovery is enabled.
func WithReplyRecovery() AgentOption { return func(a *UnifiedAgent) { a.replyRecovery = true } }

// ResumeReplyStream continues an active checkpoint without appending user input.
// Missing, terminal, corrupt and unsupported checkpoints fail before execution.
// Restored pending tools still pass through current permission/tool validation.
func (a *UnifiedAgent) ResumeReplyStream(ctx context.Context) (<-chan event.Event, error) {
	if !a.replyRecovery {
		return nil, fmt.Errorf("agent: reply recovery is not enabled")
	}
	return a.replyStream(ctx, "", true)
}

type recoveryKey struct{}
type replyRecoveryRun struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	started   bool
	coreDone  bool
	sealed    bool
	id        string
	next      int
	completed bool
	resume    bool
	failure   error
	budgetCtx context.Context
}

// A middleware may close its output before the core finishes, or never invoke
// next. Seal prevents a delayed or second invocation from executing old state;
// the exclusive lease ends only after every accepted core invocation exits.
func (r *replyRecoveryRun) startCore() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started || r.sealed {
		return false
	}
	r.started = true
	return true
}
func (a *UnifiedAgent) recoveryCoreDone(r *replyRecoveryRun) {
	r.mu.Lock()
	r.coreDone = true
	release := r.sealed
	r.mu.Unlock()
	if release {
		a.endRecovery(r)
	}
}
func (a *UnifiedAgent) sealRecovery(r *replyRecoveryRun) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.sealed = true
	r.cancel()
	release := !r.started || r.coreDone
	r.mu.Unlock()
	if release {
		a.endRecovery(r)
	}
}

func recoveryRun(ctx context.Context) *replyRecoveryRun {
	r, _ := ctx.Value(recoveryKey{}).(*replyRecoveryRun)
	return r
}
func (r *replyRecoveryRun) err() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failure
}
func (r *replyRecoveryRun) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure == nil {
		r.failure = err
	}
}
func (r *replyRecoveryRun) iteration() int     { r.mu.Lock(); defer r.mu.Unlock(); return r.next }
func (r *replyRecoveryRun) setIteration(n int) { r.mu.Lock(); defer r.mu.Unlock(); r.next = n }
func (r *replyRecoveryRun) snapshot() *ReplyRecoveryState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return &ReplyRecoveryState{Version: 1, ReplyID: r.id, Active: !r.completed, NextIteration: r.next, Budgets: *middleware.SnapshotReplyBudgets(r.budgetCtx)}
}
func (a *UnifiedAgent) beginRecovery(ctx context.Context, resume bool) (context.Context, *replyRecoveryRun, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stateSaver == nil {
		return nil, nil, fmt.Errorf("agent: reply recovery requires a StateSaver")
	}
	if a.activeRecovery != nil {
		return nil, nil, fmt.Errorf("agent: a recoverable reply is already running")
	}
	// Opt-in recovery takes ownership of nested state rather than retaining the
	// caller's checkpoint maps/slices. Existing WithState default behavior stays.
	raw, err := json.Marshal(a.state)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: invalid recovery state: %w", err)
	}
	var owned AgentState
	if err := json.Unmarshal(raw, &owned); err != nil {
		return nil, nil, err
	}
	run := &replyRecoveryRun{resume: resume}
	var budgets *middleware.ReplyBudgetSnapshot
	if resume {
		snapshot := owned.ReplyRecovery
		if owned.SchemaVersion > StateSchemaVersion || snapshot == nil || snapshot.Version != 1 || !snapshot.Active || snapshot.ReplyID == "" || snapshot.ReplyID != owned.ReplyID || snapshot.NextIteration < 0 || snapshot.NextIteration > a.reactCfg.MaxIters {
			return nil, nil, fmt.Errorf("agent: checkpoint is not a supported active reply")
		}
		run.id = snapshot.ReplyID
		run.next = snapshot.NextIteration
		budgets = &snapshot.Budgets
	}
	ctx, err = middleware.WithReplyBudgetSnapshot(ctx, budgets)
	if err != nil {
		return nil, nil, err
	}
	if resume {
		a.resetRecoveryApprovals(&owned)
	}
	ctx, run.cancel = context.WithCancel(ctx)
	run.budgetCtx = ctx
	ctx = context.WithValue(ctx, recoveryKey{}, run)
	a.state = &owned
	a.activeRecovery = run
	return ctx, run, nil
}

// Persisted approval is evidence of an earlier policy decision, not authority
// under the current policy. Re-enter permission checks for unfinished calls;
// retain recorded results and let submitted external calls use their existing
// revalidation path.
func (a *UnifiedAgent) resetRecoveryApprovals(state *AgentState) {
	for _, msg := range state.Context {
		if msg == nil || msg.Role != message.RoleAssistant || msg.Name != a.name {
			continue
		}
		finished := make(map[string]bool)
		for _, block := range msg.Content {
			if result, ok := block.(message.ToolResultBlock); ok {
				finished[result.ID] = true
			}
		}
		for i, block := range msg.Content {
			call, ok := block.(message.ToolCallBlock)
			if !ok || finished[call.ID] {
				continue
			}
			if call.State == message.ToolCallAllowed || call.State == message.ToolCallAsking {
				call.State = message.ToolCallPending
				msg.Content[i] = call
			}
		}
	}
}
func (a *UnifiedAgent) endRecovery(run *replyRecoveryRun) {
	if run == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeRecovery == run {
		a.activeRecovery = nil
	}
}
func (a *UnifiedAgent) checkpointBoundary(ctx context.Context) error {
	if !a.replyRecovery {
		a.Checkpoint(ctx)
		return nil
	}
	err := a.SaveCheckpoint(ctx)
	if err != nil {
		if run := recoveryRun(ctx); run != nil {
			run.fail(err)
		}
	}
	return err
}
func checkpointError(err error) *types.ReplyErrorInfo {
	return &types.ReplyErrorInfo{Type: types.ErrorInternal, Message: fmt.Sprintf("checkpoint failed: %v", err)}
}

// acceptRecoveryEnd runs outside middleware, before publishing an accepted
// terminal. A swallowed core candidate never marks a checkpoint completed.
func (a *UnifiedAgent) acceptRecoveryEnd(ctx context.Context, end *event.ReplyEndEvent) event.ReplyEndEvent {
	run := recoveryRun(ctx)
	if run == nil {
		return *end
	}
	if run.err() == nil {
		run.mu.Lock()
		run.completed = end.FinishedReason != types.ReplyInterrupted
		run.mu.Unlock()
		if err := a.SaveCheckpoint(ctx); err != nil {
			run.fail(err)
		}
	}
	if err := run.err(); err != nil {
		return event.NewReplyEndEventWithError(end.SessionID, end.ReplyID, types.ErrorInternal, checkpointError(err).Message)
	}
	return *end
}
