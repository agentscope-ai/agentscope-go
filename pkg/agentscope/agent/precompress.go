package agent

import (
	"context"
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
	if p := a.pendingPrecompress; p != nil {
		// A result that no longer matches the live state is dropped so a
		// fresh generation can start; a still-valid one is left for the hard
		// trigger to consume.
		if p.validLocked(a.state) {
			a.mu.Unlock()
			return
		}
		a.pendingPrecompress = nil
	}
	if a.precompressRunning || len(a.state.Context) == 0 {
		a.mu.Unlock()
		return
	}
	snapshot := make([]*message.Msg, len(a.state.Context))
	copy(snapshot, a.state.Context)
	prevSummary := a.state.Summary
	systemPrompt := a.systemPrompt
	plan := append([]tool.PlanItem(nil), a.state.Plan...)
	a.precompressRunning = true
	a.mu.Unlock()

	cfgCopy := *cfg
	// Detach cancellation: the reply's ctx dies with the reply, but the
	// snapshot stays valid for later replies (context only appends between
	// them), so the generation is allowed to finish. Values (tracing, audit)
	// carry over.
	go a.runPrecompress(context.WithoutCancel(ctx), &cfgCopy, ctxSize,
		snapshot, prevSummary, systemPrompt, plan)
}

// runPrecompress generates the summary for the snapshot. Failures are
// logged and discarded — the synchronous path owns retries and truncation
// fallbacks at the hard trigger.
func (a *UnifiedAgent) runPrecompress(ctx context.Context, cfg *ContextConfig, ctxSize int, snapshot []*message.Msg, prevSummary, systemPrompt string, plan []tool.PlanItem) {
	defer func() {
		a.mu.Lock()
		a.precompressRunning = false
		a.mu.Unlock()
	}()

	if len(a.middlewares) > 0 {
		systemPrompt = middleware.ApplySystemPromptPipeline(ctx, a.middlewares, a.name, systemPrompt)
	}
	tools := a.toolkit.GetToolSchemas()
	reserveTokens := int(float64(ctxSize) * cfg.ReserveRatio)
	if cfg.CompactStrategy == CompactStrategyFreshWindow {
		// See compressContextImpl: budget 1 routes through tool-pair repair
		// without reserving anything when nothing is in flight.
		reserveTokens = 1
	}
	baseMsgs := compressionBaseMsgs(a.name, systemPrompt, prevSummary, plan)
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
	if budget := summaryMaxTokens(cfg, ctxSize); budget > 0 {
		if truncated, did := TruncateToolResult(newSummary, budget); did {
			newSummary = truncated
		}
	}

	last := snapshot[len(snapshot)-1]
	a.mu.Lock()
	a.pendingPrecompress = &precompressResult{
		prevSummary: prevSummary,
		summary:     newSummary,
		snapshotLen: len(snapshot),
		reservedLen: len(msgsToReserve),
		lastMsg:     last,
		lastMsgID:   last.ID,
	}
	a.mu.Unlock()
	logrus.WithField("agent", a.name).Info("background pre-compression ready")
}

// validLocked reports whether the result still matches the live state: the
// summary it built upon is unchanged and the snapshot's final message still
// sits at the snapshot boundary (context only appends between rounds; a
// compaction or checkpoint rollback breaks identity). The ID comparison
// survives in-place cache clones that replace message pointers.
func (p *precompressResult) validLocked(state *AgentState) bool {
	if state.Summary != p.prevSummary || len(state.Context) < p.snapshotLen {
		return false
	}
	last := state.Context[p.snapshotLen-1]
	return last == p.lastMsg || (p.lastMsgID != "" && last.ID == p.lastMsgID)
}

// applyReadyPrecompress installs a valid background result in place of a
// synchronous compression. The reserved suffix keeps the LIVE message
// versions (not the snapshot's copies) so in-place tool-result truncation
// from overflow recovery stays in effect. Returns false when nothing
// applicable is pending.
func (a *UnifiedAgent) applyReadyPrecompress(ctx context.Context) bool {
	a.mu.Lock()
	p := a.pendingPrecompress
	if p == nil {
		a.mu.Unlock()
		return false
	}
	if !p.validLocked(a.state) {
		// Stale at the hard trigger stays stale (summary and context only
		// diverge further): drop it so later rounds can arm afresh.
		a.pendingPrecompress = nil
		a.mu.Unlock()
		return false
	}
	a.pendingPrecompress = nil
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
