package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
	"github.com/sirupsen/logrus"
)

// Thread state seams (EXPERIMENTAL). A host that runs several agents over one
// shared, append-only log (such as the thread package proposed in issue #117)
// imports log entries into an agent's context, installs summaries other
// participants wrote and tracks which entries the agent has seen. Everything
// here is opt-in: an agent whose thread slots are empty behaves exactly as
// before, on the wire and in its checkpoints (which keep their older schema
// version).

// ThreadSeqMetadataKey (EXPERIMENTAL) is the Msg.Metadata key that carries the
// thread seq of an imported thread entry (and of a reply input started
// WithThreadInput).
const ThreadSeqMetadataKey = "thread_seq"

// ThreadEventInputInjected (EXPERIMENTAL) names the CustomEvent emitted when
// boundary input (WithBoundaryInput) was applied inside a reply. Data carries
// {"cursor": uint64, "count": int}.
const ThreadEventInputInjected = "thread_input_injected"

// PeerSummary (EXPERIMENTAL) is a summary another participant's model wrote
// for this agent's window. It covers the thread entries with seq <= Through
// except the Excluded ones.
type PeerSummary struct {
	Text     string   `json:"text"`
	Origin   string   `json:"origin,omitempty"`   // e.g. "cloud/master"
	Through  uint64   `json:"through,omitempty"`  // coverage: entries with seq <= Through ...
	Excluded []uint64 `json:"excluded,omitempty"` // ... except these
}

// Covers (EXPERIMENTAL) reports whether the summary speaks for the thread
// entry seq.
func (p *PeerSummary) Covers(seq uint64) bool {
	if p == nil || seq > p.Through {
		return false
	}
	for _, x := range p.Excluded {
		if x == seq {
			return false
		}
	}
	return true
}

func (p *PeerSummary) clone() *PeerSummary {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Excluded = append([]uint64(nil), p.Excluded...)
	return &cp
}

// ThreadSeqOf (EXPERIMENTAL) returns the thread seq stored on m under
// ThreadSeqMetadataKey. It accepts unsigned and signed integers, float64 (JSON
// round trips turn a uint64 into float64), json.Number and decimal strings;
// negative, fractional and out-of-range values are rejected.
func ThreadSeqOf(m *message.Msg) (uint64, bool) {
	if m == nil || m.Metadata == nil {
		return 0, false
	}
	v, ok := m.Metadata[ThreadSeqMetadataKey]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case uint64:
		return n, true
	case uint:
		return uint64(n), true
	case uint32:
		return uint64(n), true
	case int:
		if n >= 0 {
			return uint64(n), true
		}
	case int64:
		if n >= 0 {
			return uint64(n), true
		}
	case int32:
		if n >= 0 {
			return uint64(n), true
		}
	case float64:
		// 1<<64 is exactly representable; anything at or above it overflows.
		if n >= 0 && n < math.Ldexp(1, 64) && n == math.Trunc(n) {
			return uint64(n), true
		}
	case json.Number:
		if s, err := strconv.ParseUint(string(n), 10, 64); err == nil {
			return s, true
		}
	case string:
		if s, err := strconv.ParseUint(n, 10, 64); err == nil {
			return s, true
		}
	}
	return 0, false
}

// threadPromptMsgs builds the thread message prepareModelInput injects after
// the agent's own summary: the peer summary. It returns nil when the slot is
// empty, so agents that never use threads send the same requests as before.
func threadPromptMsgs(agentName string, peer *PeerSummary) []*message.Msg {
	if peer == nil || peer.Text == "" {
		return nil
	}
	origin := peer.Origin
	if origin == "" {
		origin = "peer"
	}
	return []*message.Msg{message.UserMsg(agentName, "[Summary from "+origin+
		", covering the shared thread through #"+strconv.FormatUint(peer.Through, 10)+"]:\n"+peer.Text)}
}

// ThreadUpdate (EXPERIMENTAL) is one atomic change to an agent's thread state.
type ThreadUpdate struct {
	// Reset clears Context, the agent's own Summary, the read cache, any
	// pending background precompression and the provider-grounded token
	// floor (lastModelInputTokens), before anything else applies. Permission
	// grants, Plan and the toolkit are kept. Only allowed between replies.
	Reset bool
	// PeerSummary, when non-nil, replaces the slot. Context messages that
	// carry a thread seq covered by it (seq <= Through and not Excluded) are
	// removed: the summary now speaks for them. Messages without a thread seq
	// (the agent's own turns) are never removed. Removal must not split a
	// tool_call/tool_result pair; imported messages never hold tool blocks,
	// so removing whole imported messages is safe. (A seq-tagged message that
	// does hold tool blocks is kept.) Its Text must not be empty or blank.
	PeerSummary *PeerSummary
	// ClearPeerSummary removes the slot (used by tests and resets). It may not
	// be combined with PeerSummary.
	ClearPeerSummary bool
	// Messages are appended after the removal. They follow Observe's rules
	// (no system role, no tool_call, tool_result or thinking blocks) and
	// should carry ThreadSeqMetadataKey. The agent owns them afterwards.
	Messages []*message.Msg
	// Cursor is the new ThreadCursor; zero keeps the current value. It may
	// not move backwards.
	Cursor uint64
}

// BoundaryInput (EXPERIMENTAL) supplies thread updates inside a running reply.
// It is called at every certified StateReason boundary (where no tool call
// awaits its result) before the model call; a nil update means nothing to
// apply.
type BoundaryInput func(ctx context.Context) (*ThreadUpdate, error)

// WithBoundaryInput installs src as the agent's boundary input (EXPERIMENTAL).
// Each applied update emits a ThreadEventInputInjected CustomEvent and is
// persisted before the model call: through the reply-recovery checkpoint when
// recovery is enabled, otherwise through SaveCheckpoint when a StateSaver is
// configured. An error from src, an invalid update (Reset is never allowed
// inside a reply) or a persistence failure ends the reply as an error.
func WithBoundaryInput(src BoundaryInput) AgentOption {
	return func(a *UnifiedAgent) { a.boundaryInput = src }
}

type threadInputKey struct{}

// WithThreadInput marks the input of the reply started with ctx as the thread
// entry seq (EXPERIMENTAL): the user message the reply appends carries
// ThreadSeqMetadataKey, and ThreadCursor is raised to seq (never lowered) in
// the same state update. A resumed reply appends no message, so it ignores
// the mark. Middleware must wrap, not replace, the reply context for the
// mark to reach the agent.
func WithThreadInput(ctx context.Context, seq uint64) context.Context {
	return context.WithValue(ctx, threadInputKey{}, seq)
}

func threadInputFrom(ctx context.Context) (uint64, bool) {
	seq, ok := ctx.Value(threadInputKey{}).(uint64)
	return seq, ok
}

// tagThreadInputLocked stamps a reply's user message with its thread seq and
// raises the cursor. Caller holds a.mu.
func (a *UnifiedAgent) tagThreadInputLocked(m *message.Msg, seq uint64) {
	if m.Metadata == nil {
		m.Metadata = types.JSONObject{}
	}
	m.Metadata[ThreadSeqMetadataKey] = seq
	if seq > a.state.ThreadCursor {
		a.state.ThreadCursor = seq
	}
}

// ThreadView (EXPERIMENTAL) is a copy of an agent's thread state.
type ThreadView struct {
	Cursor          uint64
	PeerSummary     *PeerSummary
	ContextMessages int
	// InputTokens estimates the model input the next call would send (the
	// system prompt with state awareness and skill instructions, summaries,
	// thread messages, plan, context and tool schemas): the model's
	// CountTokens estimate, floored by the provider-reported input of the
	// last successful call. It is an estimate only. It uses the raw system
	// prompt: OnSystemPrompt middleware is not run, so prompt changes a
	// middleware makes are not counted.
	InputTokens int
	// Window is the context size the compression path works against
	// (ContextConfig.ContextSize, else the model's resolved window, the
	// smaller of the model and its fallback). It is 0 when neither the
	// configuration nor the model reports one.
	Window int
	// SummaryTokens is the ThreadSummaryTokens budget that applies while a
	// peer summary is held: the value set (configured or
	// SetThreadSummaryTokens), or with 0 the automatic one for Window. It is
	// reported before a peer summary is held as well, so a host can size the
	// first one from it, although the automatic budget bounds nothing until
	// then. It is 0 when the option is off, or when the automatic budget
	// applies and the window is unknown.
	SummaryTokens int
	// CompactAt is the estimated input at which automatic compression fires:
	// TriggerRatio x Window. It is 0 when Window is unknown or the agent has
	// no ContextConfig (no automatic compression).
	CompactAt int
}

// ThreadView (EXPERIMENTAL) returns a copy of the agent's thread state. It may
// be called while a reply runs: state is copied under the lock and tokens are
// counted on the copies afterwards. It runs no middleware (see InputTokens).
func (a *UnifiedAgent) ThreadView() ThreadView {
	a.mu.Lock()
	v := ThreadView{
		Cursor:          a.state.ThreadCursor,
		PeerSummary:     a.state.PeerSummary.clone(),
		ContextMessages: len(a.state.Context),
	}
	input := cloneMsgs(a.modelInputLocked(a.systemPrompt))
	floor := a.lastModelInputTokens
	var cfgSize, summaryTokens int
	var trigger float64
	if a.contextCfg != nil {
		cfgSize = a.contextCfg.ContextSize
		summaryTokens = a.contextCfg.ThreadSummaryTokens
		trigger = a.contextCfg.withDefaults().TriggerRatio
	}
	if a.threadSummarySet {
		summaryTokens = a.threadSummaryTokens
	}
	a.mu.Unlock()

	v.InputTokens = max(a.model.CountTokens(input, a.toolkit.GetToolSchemas()), floor)
	v.Window = a.knownWindow(cfgSize)
	v.CompactAt = int(float64(v.Window) * trigger)
	switch {
	case summaryTokens > 0:
		v.SummaryTokens = summaryTokens
	case summaryTokens == 0:
		v.SummaryTokens = v.Window * threadSummaryAutoNum / threadSummaryAutoDen
	}
	return v
}

// knownWindow resolves the window the compression path uses, or 0 when no
// size is configured and the model reports none (the compression path would
// then assume defaultContextSize).
func (a *UnifiedAgent) knownWindow(cfgSize int) int {
	if cfgSize > 0 {
		return cfgSize
	}
	if model.ResolveContextSize(a.model, 0) <= 0 {
		return 0
	}
	size := model.ResolveContextSize(a.model, defaultContextSize)
	if a.modelCfg.FallbackModel != nil {
		size = min(size, model.ResolveContextSize(a.modelCfg.FallbackModel, defaultContextSize))
	}
	return size
}

// cloneMsgs copies each message and its Content slice, so the copies can be
// read while the reply goroutine keeps appending to or updating the live ones.
// Blocks are values; nested maps are shared and treated as read-only.
func cloneMsgs(msgs []*message.Msg) []*message.Msg {
	if msgs == nil {
		return nil
	}
	out := make([]*message.Msg, len(msgs))
	for i, m := range msgs {
		if m == nil {
			continue
		}
		cp := *m
		cp.Content = append([]message.ContentBlock(nil), m.Content...)
		out[i] = &cp
	}
	return out
}

// ErrReplyInProgress is wrapped by the error a between-replies operation
// (ApplyThreadUpdate, ApplySummary, CompactStrict) returns while
// a reply is still in flight (EXPERIMENTAL): a caller may wait and retry.
var ErrReplyInProgress = errors.New("a reply is in progress")

// busyLocked rejects a between-replies operation while the agent is running a
// reply or owned by a Conversation. A reply counts as running until both its
// event stream has ended (activeReplies) and its reply core has returned
// (runningCores): the stream can end first when a middleware stops
// forwarding on cancellation while the core is still inside a tool. Caller
// holds a.mu.
func (a *UnifiedAgent) busyLocked(op string) error {
	if a.activeReplies > 0 || a.runningCores > 0 {
		return fmt.Errorf("agent %s: %s requires an idle agent; %w", a.name, op, ErrReplyInProgress)
	}
	if a.activeConv != nil {
		return fmt.Errorf("agent %s: %s is not allowed while a conversation owns the agent", a.name, op)
	}
	return nil
}

// validateThreadUpdate checks the parts of u that do not depend on state.
func validateThreadUpdate(u *ThreadUpdate) error {
	if u.PeerSummary != nil && u.ClearPeerSummary {
		return errors.New("agent: thread update: PeerSummary and ClearPeerSummary are mutually exclusive")
	}
	// A summary without text would remove the messages it covers while
	// injecting nothing in their place.
	if u.PeerSummary != nil && strings.TrimSpace(u.PeerSummary.Text) == "" {
		return errors.New("agent: thread update: PeerSummary has no text; use ClearPeerSummary to remove the slot")
	}
	for i, m := range u.Messages {
		if m == nil {
			continue
		}
		if err := validateObservedMsg(m); err != nil {
			return fmt.Errorf("agent: thread update: message %d: %w", i, err)
		}
	}
	return nil
}

// validateObservedMsg applies Observe's rules to one message.
func validateObservedMsg(m *message.Msg) error {
	if m.Role == message.RoleSystem {
		return errors.New("system-role messages are not allowed")
	}
	if len(m.GetContentBlocks(message.ContentBlockToolCall)) > 0 {
		return errors.New("messages containing tool_call blocks are not allowed")
	}
	if len(m.GetContentBlocks(message.ContentBlockToolResult)) > 0 {
		return errors.New("messages containing tool_result blocks are not allowed")
	}
	if len(m.GetContentBlocks(message.ContentBlockThinking)) > 0 {
		return errors.New("messages containing thinking blocks are not allowed")
	}
	return nil
}

// checkCursorLocked rejects a cursor that would move backwards.
func (a *UnifiedAgent) checkCursorLocked(cursor uint64) error {
	if cursor != 0 && cursor < a.state.ThreadCursor {
		return fmt.Errorf("agent: thread update: cursor %d is behind the current cursor %d", cursor, a.state.ThreadCursor)
	}
	return nil
}

// applyThreadUpdateLocked applies a validated update. Caller holds a.mu.
func (a *UnifiedAgent) applyThreadUpdateLocked(u *ThreadUpdate) {
	if u.Reset {
		a.state.Context = nil
		a.state.Summary = ""
		a.state.ReadCacheData = nil
		a.dropReadyLocked()
		a.lastModelInputTokens = 0
		a.lastCapture = nil
		a.compactionVersion++
		if a.readCache != nil {
			a.readCache.CleanFileCache(nil)
		}
	}
	if u.ClearPeerSummary {
		a.state.PeerSummary = nil
	}
	if u.PeerSummary != nil {
		ps := u.PeerSummary.clone()
		a.state.PeerSummary = ps
		a.boundOwnSummaryLocked()
		if a.removeCoveredLocked(ps) {
			// The provider-grounded floor measured the larger context; keep it
			// from triggering a compaction of the shrunken one.
			a.lastModelInputTokens = 0
		}
	}
	for _, m := range u.Messages {
		if m != nil {
			a.state.Context = append(a.state.Context, m)
		}
	}
	if u.Cursor != 0 {
		a.state.ThreadCursor = u.Cursor
	}
}

// removeCoveredLocked drops context messages whose thread seq the summary
// covers and reports whether anything was removed. The context slice is
// rebuilt rather than filtered in place: earlier snapshots may share it.
func (a *UnifiedAgent) removeCoveredLocked(ps *PeerSummary) bool {
	kept := make([]*message.Msg, 0, len(a.state.Context))
	for _, m := range a.state.Context {
		if seq, ok := ThreadSeqOf(m); ok && ps.Covers(seq) && !holdsToolBlocks(m) {
			continue
		}
		kept = append(kept, m)
	}
	if len(kept) == len(a.state.Context) {
		return false
	}
	a.state.Context = kept
	return true
}

func holdsToolBlocks(m *message.Msg) bool {
	for _, b := range m.Content {
		switch b.(type) {
		case message.ToolCallBlock, message.ToolResultBlock:
			return true
		}
	}
	return false
}

// ApplyThreadUpdate applies u between replies (EXPERIMENTAL). It returns an
// error and changes nothing while a reply is in flight (its event stream is
// open or its reply core has not returned yet; a canceled reply's stream can
// close while the core still waits for a tool), while a Conversation owns the
// agent, or when u is invalid (see ThreadUpdate). The update is
// applied in one critical section and then persisted through SaveCheckpoint
// when a StateSaver is configured, so cursor and content land in one
// checkpoint. If persisting fails the error is returned, but the update stays
// applied in memory; retry persistence with SaveCheckpoint.
func (a *UnifiedAgent) ApplyThreadUpdate(ctx context.Context, u ThreadUpdate) error { //nolint:gocritic // the value parameter keeps the documented API
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateThreadUpdate(&u); err != nil {
		return err
	}
	a.mu.Lock()
	if err := a.busyLocked("thread update"); err != nil {
		a.mu.Unlock()
		return err
	}
	if err := a.checkCursorLocked(u.Cursor); err != nil {
		a.mu.Unlock()
		return err
	}
	a.applyThreadUpdateLocked(&u)
	a.mu.Unlock()
	return a.persistBetweenReplies(ctx)
}

// persistBetweenReplies saves a checkpoint when a StateSaver is configured.
func (a *UnifiedAgent) persistBetweenReplies(ctx context.Context) error {
	if a.stateSaver == nil {
		return nil
	}
	return a.SaveCheckpoint(ctx)
}

// drainBoundaryInput applies the boundary input at a certified StateReason
// boundary. It returns how to end the reply, or "" to go on: interrupted when
// the source failed because the reply was canceled, else an error end with
// its info.
func (a *UnifiedAgent) drainBoundaryInput(ctx context.Context, ch chan<- event.Event, replyID string) (types.ReplyFinishedReason, *types.ReplyErrorInfo) {
	u, err := a.boundaryInput(ctx)
	if err != nil {
		// Canceled after the loop's own check: a source that returns on
		// cancellation has not failed.
		if ctx.Err() != nil {
			return types.ReplyInterrupted, nil
		}
		return types.ReplyError, threadInputError(err)
	}
	if u == nil {
		return "", nil
	}
	if u.Reset {
		return types.ReplyError, threadInputError(errors.New("reset is not allowed inside a reply"))
	}
	if err := validateThreadUpdate(u); err != nil {
		return types.ReplyError, threadInputError(err)
	}
	a.mu.Lock()
	if err := a.checkCursorLocked(u.Cursor); err != nil {
		a.mu.Unlock()
		return types.ReplyError, threadInputError(err)
	}
	a.applyThreadUpdateLocked(u)
	cursor := a.state.ThreadCursor
	a.mu.Unlock()
	emit(ctx, ch, event.NewCustomEvent(replyID, ThreadEventInputInjected, map[string]any{
		"cursor": cursor,
		"count":  len(u.Messages),
	}))
	// Persist cursor and content together before the model sees them: with
	// reply recovery through the fail-stop boundary checkpoint, otherwise
	// through SaveCheckpoint (whose failure also ends the reply).
	if a.replyRecovery {
		if err := a.checkpointBoundary(ctx); err != nil {
			return types.ReplyError, checkpointError(err)
		}
	} else if a.stateSaver != nil {
		if err := a.SaveCheckpoint(ctx); err != nil {
			return types.ReplyError, checkpointError(err)
		}
	}
	return "", nil
}

func threadInputError(err error) *types.ReplyErrorInfo {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "agent: thread update: ")
	return &types.ReplyErrorInfo{Type: types.ErrorInternal, Message: "thread input failed: " + msg}
}

// minOwnSummaryTokens is the smallest own-summary share ThreadSummaryTokens
// leaves: the smallest size the agent's overflow recovery elides a text to.
const minOwnSummaryTokens = 256

// threadSummaryAuto is the share of the context window the automatic
// ThreadSummaryTokens budget takes: 3/8, which the 3,000 tokens of an
// 8,192-token window used in the issue #117 experiments round to.
const threadSummaryAutoNum, threadSummaryAutoDen = 3, 8

// SetThreadSummaryTokens (EXPERIMENTAL) changes
// ContextConfig.ThreadSummaryTokens for what follows: a positive value is the
// budget, 0 the automatic one, a negative value turns the option off. An own
// summary over its new share is elided at once. It is safe for concurrent use.
func (a *UnifiedAgent) SetThreadSummaryTokens(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.threadSummaryTokens, a.threadSummarySet = n, true
	a.boundOwnSummaryLocked()
}

// threadSummaryTotalLocked is the ThreadSummaryTokens budget in force, 0
// when the option is off: the value set at run time, else the config's; a
// positive value as is; zero 3/8 of ctxSize while a peer summary is held.
// Caller holds a.mu.
func (a *UnifiedAgent) threadSummaryTotalLocked(cfg *ContextConfig, ctxSize int) int {
	n := 0
	if cfg != nil {
		n = cfg.ThreadSummaryTokens
	}
	if a.threadSummarySet {
		n = a.threadSummaryTokens
	}
	switch {
	case n > 0:
		return n
	case n < 0, a.state.PeerSummary == nil, ctxSize <= 0:
		return 0
	}
	return ctxSize * threadSummaryAutoNum / threadSummaryAutoDen
}

// threadContextSizeLocked is the window the automatic budget is sized from:
// the configured ContextSize, else the smaller of the model's and its
// fallback's. Caller holds a.mu.
func (a *UnifiedAgent) threadContextSizeLocked() int {
	if a.contextCfg != nil && a.contextCfg.ContextSize > 0 {
		return a.contextCfg.ContextSize
	}
	n := model.ResolveContextSize(a.model, defaultContextSize)
	if a.modelCfg.FallbackModel != nil {
		n = min(n, model.ResolveContextSize(a.modelCfg.FallbackModel, defaultContextSize))
	}
	return n
}

// ownSummaryShare is the own summary's share of total next to peer, or 0
// when total is 0. It counts in TruncateToolResult's unit.
func ownSummaryShare(total int, peer *PeerSummary) int {
	if total <= 0 {
		return 0
	}
	left := total
	if peer != nil {
		left -= len(peer.Text) / 4
	}
	return max(left, minOwnSummaryTokens)
}

// ownSummaryBudget is the budget a newly written own summary is elided to:
// the SummaryMaxTokens budget, lowered to its ThreadSummaryTokens share.
func (a *UnifiedAgent) ownSummaryBudget(cfg *ContextConfig, ctxSize int) int {
	budget := summaryMaxTokens(cfg, ctxSize)
	a.mu.Lock()
	share := ownSummaryShare(a.threadSummaryTotalLocked(cfg, ctxSize), a.state.PeerSummary)
	a.mu.Unlock()
	if share > 0 && (budget <= 0 || share < budget) {
		budget = share
	}
	return budget
}

// reserveTokens is the reserve a compression keeps for the recent tail. The
// split charges the thread message (the peer summary) with the rest of the
// head; while a ThreadSummaryTokens budget is in force it is given back, so a
// held peer summary does not leave the agent without recent turns. The
// reserve then stays under 0.75 of the window and under the trigger.
func (a *UnifiedAgent) reserveTokens(cfg *ContextConfig, ctxSize int) int {
	reserve := int(float64(ctxSize) * cfg.ReserveRatio)
	a.mu.Lock()
	total := a.threadSummaryTotalLocked(cfg, ctxSize)
	thread := threadPromptMsgs(a.name, a.state.PeerSummary)
	a.mu.Unlock()
	if total <= 0 || len(thread) == 0 {
		return reserve
	}
	limit := int(float64(ctxSize) * min(0.75, cfg.TriggerRatio-0.05))
	return max(reserve, min(reserve+a.model.CountTokens(thread, nil), limit))
}

// boundOwnSummaryLocked elides the own summary to its ThreadSummaryTokens
// share next to the peer summary just set (or the budget just changed): the
// share is otherwise applied only when the next summary is written, and the
// two would exceed the total until then. Caller holds a.mu.
func (a *UnifiedAgent) boundOwnSummaryLocked() {
	if a.contextCfg == nil || a.state.Summary == "" {
		return
	}
	share := ownSummaryShare(a.threadSummaryTotalLocked(a.contextCfg, a.threadContextSizeLocked()), a.state.PeerSummary)
	if share <= 0 {
		return
	}
	if text, did := truncateSummary(a.state.Summary, share); did {
		a.state.Summary = text
		logrus.WithFields(logrus.Fields{"agent": a.name, "budget": share}).
			Warn("own summary exceeded its share next to the peer summary; middle-elided")
	}
}
