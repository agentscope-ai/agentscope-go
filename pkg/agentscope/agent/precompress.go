package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// Background pre-compression (ContextConfig.PrecompressRatio): once the
// estimated context crosses the precompress ratio, the summary for the
// current context snapshot is generated off the reply goroutine. When the
// hard trigger later fires, a still-valid result applies instantly; anything
// else falls back to the synchronous compression path. The generation burns
// tokens like any compression call — its usage flows through
// recordCompressionUsage and is drained by the reply loop.

// precompressResult is a background-generated summary awaiting application.
// The summary covers the snapshot prefix [0, snapshotLen-reservedLen); the
// reserved suffix and any messages appended since the snapshot stay raw.
type precompressResult struct {
	prevSummary string // state.Summary the generation built upon
	summary     string
	snapshotLen int
	reservedLen int          // suffix of the snapshot kept raw
	lastMsg     *message.Msg // boundary identity: snapshot's final message
	lastMsgID   string
	prefix      string // contextDigest of the snapshot
	offer       bool   // from a SummaryOffer
}

// contextDigest fingerprints the order of msgs by each message's role, name,
// ID and thread seq. Appends after msgs, in-place clones and blocks added to
// a message keep it; removing, inserting or replacing a message changes it.
// The boundary message's ID alone cannot tell a removal before it: every
// assistant message of one reply carries the reply ID, so when input splits
// a reply and a peer summary later removes thread entries, a later message of
// the same reply can slide onto the boundary index.
//
// Each field is length-prefixed, so no value can shift a field boundary. The
// seq is read through ThreadSeqOf, which also accepts the float64 a JSON
// reload produces, so a digest survives a checkpoint round trip; a seq above
// 2^53 does not survive it exactly, and an offer over such an entry is then
// dropped as stale.
func contextDigest(msgs []*message.Msg) string {
	h := sha256.New()
	buf := make([]byte, 0, 128)
	for _, m := range msgs {
		buf = buf[:0]
		if m == nil {
			buf = append(buf, 0)
		} else {
			buf = append(buf, 1)
			for _, f := range [...]string{string(m.Role), m.Name, m.ID} {
				buf = binary.AppendUvarint(buf, uint64(len(f)))
				buf = append(buf, f...)
			}
			if n, ok := ThreadSeqOf(m); ok {
				buf = binary.BigEndian.AppendUint64(append(buf, 1), n)
			} else {
				buf = append(buf, 0)
			}
		}
		h.Write(buf)
	}
	var sum [sha256.Size]byte
	return hex.EncodeToString(h.Sum(sum[:0]))
}

// maybeArmPrecompress starts a background summary generation when the
// estimate has crossed the precompress ratio and no usable result is running
// or ready. Called from compressContextImpl on the under-threshold path, so
// by construction the hard trigger has not fired.
func (a *UnifiedAgent) maybeArmPrecompress(ctx context.Context, cfg *ContextConfig, ctxSize, estimatedTokens int) {
	ratio := cfg.PrecompressRatio
	if ratio <= 0 || ratio >= cfg.TriggerRatio {
		return
	}
	// The block-level splitter has different granularity; mixing a
	// message-level background split with block-level application would make
	// the outcome depend on a race. Precompression stays off under it.
	if a.contentBlockCompaction {
		return
	}
	if estimatedTokens < int(float64(ctxSize)*ratio) {
		return
	}

	a.mu.Lock()
	if p := a.readyLocked(); p != nil {
		// A result that no longer matches the live state is dropped so a
		// fresh generation can start; a still-valid one is left for the hard
		// trigger to consume.
		if p.validLocked(a.state) {
			a.mu.Unlock()
			return
		}
		a.dropReadyLocked()
	}
	if a.precompressRunning || len(a.state.Context) == 0 {
		a.mu.Unlock()
		return
	}
	snapshot := make([]*message.Msg, len(a.state.Context))
	copy(snapshot, a.state.Context)
	prefix := contextDigest(snapshot)
	prevSummary := a.state.Summary
	systemPrompt := a.systemPrompt
	plan := append([]tool.PlanItem(nil), a.state.Plan...)
	thread := threadPromptMsgs(a.name, a.state.PeerSummary)
	a.precompressRunning = true
	a.mu.Unlock()

	cfgCopy := *cfg
	// Detach cancellation: the reply's ctx dies with the reply, but the
	// snapshot stays valid for later replies while the context only grows
	// past it, so the generation is allowed to finish. Values (tracing, audit)
	// carry over.
	go a.runPrecompress(context.WithoutCancel(ctx), &cfgCopy, ctxSize,
		snapshot, prefix, prevSummary, systemPrompt, thread, plan)
}

// runPrecompress generates the summary for the snapshot. Failures are
// logged and discarded — the synchronous path owns retries and truncation
// fallbacks at the hard trigger.
func (a *UnifiedAgent) runPrecompress(ctx context.Context, cfg *ContextConfig, ctxSize int, snapshot []*message.Msg, prefix, prevSummary, systemPrompt string, thread []*message.Msg, plan []tool.PlanItem) {
	defer func() {
		a.mu.Lock()
		a.precompressRunning = false
		a.mu.Unlock()
	}()

	if len(a.middlewares) > 0 {
		systemPrompt = middleware.ApplySystemPromptPipeline(ctx, a.middlewares, a.name, systemPrompt)
	}
	tools := a.toolkit.GetToolSchemas()
	reserveTokens := a.reserveTokens(cfg, ctxSize)
	if cfg.CompactStrategy == CompactStrategyFreshWindow {
		// See compressContextImpl: budget 1 routes through tool-pair repair
		// without reserving anything when nothing is in flight.
		reserveTokens = 1
	}
	baseMsgs := compressionBaseMsgs(a.name, systemPrompt, prevSummary, thread, plan)
	msgsToCompress, msgsToReserve := a.splitMsgsForCompression(snapshot, baseMsgs, reserveTokens, tools)
	if len(msgsToCompress) == 0 {
		return
	}

	compressionMsgs := buildCompressionMessages(systemPrompt, prevSummary, msgsToCompress, cfg.CompressionPrompt)
	result, compUsage, err := model.GenerateStructuredOutputWithUsage(ctx, a.model, compressionMsgs, cfg.SummarySchema)
	a.recordCompressionUsage(compUsage)
	if err != nil {
		logrus.WithError(err).WithField("agent", a.name).Warn("background pre-compression failed")
		return
	}
	newSummary, err := formatSummary(cfg.SummaryTemplate, result)
	if err != nil {
		logrus.WithError(err).WithField("agent", a.name).Warn("background pre-compression formatting failed")
		return
	}
	// A blank summary (a custom template over empty fields) would empty the
	// history if applied; it is discarded like a failed generation, so the
	// next round may try again and the hard trigger compresses synchronously.
	if strings.TrimSpace(newSummary) == "" {
		logrus.WithField("agent", a.name).Warn("background pre-compression produced an empty summary")
		return
	}
	if budget := a.ownSummaryBudget(cfg, ctxSize); budget > 0 {
		if truncated, did := truncateSummary(newSummary, budget); did {
			newSummary = truncated
		}
	}

	last := snapshot[len(snapshot)-1]
	a.mu.Lock()
	// A generation starts only when nothing valid waits, so a valid result
	// waiting now is an offer made while this one ran: it is newer and wins.
	if p := a.readyLocked(); p != nil && p.validLocked(a.state) {
		a.mu.Unlock()
		logrus.WithField("agent", a.name).Debug("background pre-compression discarded: a newer offer is waiting")
		return
	}
	// Otherwise the background result replaces whatever waited.
	a.dropReadyLocked()
	a.pendingPrecompress = &precompressResult{
		prevSummary: prevSummary,
		summary:     newSummary,
		snapshotLen: len(snapshot),
		reservedLen: len(msgsToReserve),
		lastMsg:     last,
		lastMsgID:   last.ID,
		prefix:      prefix,
	}
	a.mu.Unlock()
	logrus.WithField("agent", a.name).Info("background pre-compression ready")
}

// readyLocked returns the summary waiting for the trigger: the pending
// background result or offer, else the offer the state carries after a
// reload. Caller holds a.mu.
func (a *UnifiedAgent) readyLocked() *precompressResult {
	if a.pendingPrecompress == nil && a.state.SummaryOffer != nil {
		if a.contentBlockCompaction {
			// Offers are off under the block-level splitter (OfferSummary);
			// one loaded from a checkpoint is dropped.
			logrus.WithField("agent", a.name).Warn("summary offer dropped: offers are off with content-block compaction")
			a.state.SummaryOffer = nil
			return nil
		}
		a.pendingPrecompress = a.state.SummaryOffer.result()
	}
	return a.pendingPrecompress
}

// dropReadyLocked drops the summary waiting for the trigger, the state's
// offer included. Caller holds a.mu.
func (a *UnifiedAgent) dropReadyLocked() {
	a.pendingPrecompress = nil
	a.state.SummaryOffer = nil
}

// validLocked reports whether the result still matches the live state: the
// summary it built upon is unchanged, the snapshot's final message still sits
// at the snapshot boundary and the messages up to it are the snapshot's, in
// order (contextDigest). Appends past the boundary keep a result valid; a
// compaction, a checkpoint rollback or a removal before the boundary does
// not. The ID comparisons survive in-place cache clones that replace message
// pointers and a reload from a checkpoint.
func (p *precompressResult) validLocked(state *AgentState) bool {
	// An offer can come from a checkpoint the agent did not write: reject
	// bounds that would index outside the context, and an empty summary.
	if p.snapshotLen <= 0 || p.reservedLen < 0 || p.reservedLen > p.snapshotLen || strings.TrimSpace(p.summary) == "" {
		return false
	}
	// OfferSummary always keeps the boundary message, which the reply may
	// have grown since the snapshot; an offer keeping none is not its own.
	if p.offer && p.reservedLen < 1 {
		return false
	}
	if state.Summary != p.prevSummary || len(state.Context) < p.snapshotLen {
		return false
	}
	last := state.Context[p.snapshotLen-1]
	if last == nil {
		// A loaded offer carries no message pointer, so a nil boundary
		// message would otherwise match it.
		return false
	}
	if last != p.lastMsg && (p.lastMsgID == "" || last.ID != p.lastMsgID) {
		return false
	}
	// A result without a digest (an offer from a checkpoint written without
	// one) cannot prove its prefix, so it is not applied.
	return p.prefix != "" && contextDigest(state.Context[:p.snapshotLen]) == p.prefix
}

// applyReadyPrecompress installs a valid background result in place of a
// synchronous compression. The reserved suffix keeps the LIVE message
// versions (not the snapshot's copies) so in-place tool-result truncation
// from overflow recovery stays in effect. Returns false when nothing
// applicable is pending.
func (a *UnifiedAgent) applyReadyPrecompress(ctx context.Context) bool {
	a.mu.Lock()
	p := a.readyLocked()
	if p == nil {
		a.mu.Unlock()
		return false
	}
	if !p.validLocked(a.state) {
		// Stale at the hard trigger stays stale (summary and context only
		// diverge further): drop it so later rounds can arm afresh.
		a.dropReadyLocked()
		a.mu.Unlock()
		return false
	}
	a.dropReadyLocked()
	a.mu.Unlock()

	newSummary := p.summary
	// Offload at apply time (not generation): only a summary that is actually
	// installed leaves a workspace artifact.
	if a.offloader != nil {
		path, offErr := a.offloader.OffloadContent(ctx, newSummary, "compressed_context.txt")
		if offErr != nil {
			logrus.WithError(offErr).WithField("agent", a.name).Warn("failed to offload compressed context")
		} else {
			newSummary += fmt.Sprintf(
				"\n<system-reminder>The compressed context is offloaded to '%s'.</system-reminder>",
				path,
			)
		}
	}

	a.mu.Lock()
	// Re-validate: nothing is expected to have mutated state between the two
	// lock sections, but a stale apply is worse than a wasted generation.
	if !p.validLocked(a.state) {
		a.mu.Unlock()
		return false
	}
	reserved := a.state.Context[p.snapshotLen-p.reservedLen:]
	newCtx := make([]*message.Msg, len(reserved))
	copy(newCtx, reserved)
	a.state.Context = newCtx
	a.state.Summary = newSummary
	// The actual-usage floor measured the pre-compaction context; keep it
	// from re-triggering compaction against the shrunken context.
	a.lastModelInputTokens = 0
	a.mu.Unlock()

	if a.readCache != nil {
		cleanReadCacheForReserved(a.readCache, reserved)
	}

	logrus.WithFields(logrus.Fields{
		"agent":    a.name,
		"reserved": len(reserved),
	}).Info("background pre-compression applied")
	return true
}
