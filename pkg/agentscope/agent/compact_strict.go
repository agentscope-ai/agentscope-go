package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// CompactStrictOptions (EXPERIMENTAL) configures CompactStrict.
type CompactStrictOptions struct {
	Prompt string // replaces ContextConfig.CompressionPrompt for this call when non-empty
}

// errStrictStale reports that the context changed while a strict compaction
// generated its summary.
var errStrictStale = errors.New("agent: strict compaction: the context changed during summary generation")

// CompactStrict (EXPERIMENTAL) compresses the conversation now, like Compact,
// but never degrades: any failure (generation error, cancellation, an
// unusable, unformattable or empty summary, or a context changed meanwhile)
// returns the error and leaves the state exactly as before. There is no
// truncation fallback and no retry that drops the oldest messages (contrast
// Compact's fallbackTruncateKeepSummary and retryCompressWithFewer), and
// images are not limited before the summary succeeds.
//
// It requires WithContextConfig, runs through the OnCompressContext
// middleware chain like Compact, and may only be called between replies: it
// fails while a reply is in flight (including a reply core still running
// after its stream closed) or a Conversation owns the agent. It reports
// whether the context changed; a change is saved through the
// StateSaver, when one is configured, like ApplyThreadUpdate does, and a
// failed save is returned with changed still true. Compression usage is
// recorded as on the automatic path and reported by the next reply. A
// still-valid background precompression result applies instantly unless
// opts.Prompt asks for a different summary.
func (a *UnifiedAgent) CompactStrict(ctx context.Context, opts CompactStrictOptions) (bool, error) {
	if ctx == nil {
		return false, errors.New("agent: strict compaction: nil context")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	a.mu.Lock()
	cfgPtr := a.contextCfg
	busy := a.busyLocked("strict compaction")
	prevSummary := a.state.Summary
	prevLen := len(a.state.Context)
	prevVersion := a.compactionVersion
	a.mu.Unlock()
	if busy != nil {
		return false, busy
	}
	if cfgPtr == nil {
		return false, fmt.Errorf("agent %s: context compression is not configured; add WithContextConfig to enable it", a.name)
	}
	base := *cfgPtr
	if opts.Prompt != "" {
		base.CompressionPrompt = opts.Prompt
	}

	var err error
	if len(a.middlewares) == 0 {
		cfg := base
		err = a.compactStrictImpl(ctx, &cfg, opts.Prompt != "")
	} else {
		core := func(ctx context.Context, input middleware.CompressInput) error {
			cfg := base
			cfg.TriggerRatio = input.TriggerRatio
			cfg.ReserveRatio = input.ReserveRatio
			return a.compactStrictImpl(ctx, &cfg, opts.Prompt != "")
		}
		err = middleware.BuildCompressChain(a.middlewares, core)(ctx, middleware.CompressInput{
			AgentName:    a.name,
			TriggerRatio: base.TriggerRatio,
			ReserveRatio: base.ReserveRatio,
		})
	}
	if err == nil {
		err = ctx.Err()
	}
	a.mu.Lock()
	changed := a.state.Summary != prevSummary || len(a.state.Context) != prevLen || a.compactionVersion != prevVersion
	a.mu.Unlock()
	if changed {
		// Like ApplyThreadUpdate and ApplySummary, save the change at once so
		// an agent restored from the checkpoint starts from the compacted
		// state.
		if perr := a.persistBetweenReplies(ctx); perr != nil && err == nil {
			err = fmt.Errorf("agent %s: strict compaction: save the compacted state: %w", a.name, perr)
		}
	}
	return changed, err
}

// compactStrictImpl is compressContextImpl's forced path without fallbacks.
// State is written once, at the end, and only after every step succeeded.
func (a *UnifiedAgent) compactStrictImpl(ctx context.Context, cfg *ContextConfig, customPrompt bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctxSize := cfg.ContextSize
	if ctxSize == 0 {
		ctxSize = model.ResolveContextSize(a.model, defaultContextSize)
		if a.modelCfg.FallbackModel != nil {
			ctxSize = min(ctxSize, model.ResolveContextSize(a.modelCfg.FallbackModel, defaultContextSize))
		}
	}

	a.mu.Lock()
	contextLen := len(a.state.Context)
	var contextLast *message.Msg
	if contextLen > 0 {
		contextLast = a.state.Context[contextLen-1]
	}
	a.mu.Unlock()
	if contextLen == 0 {
		return nil // nothing to summarize
	}

	// A finished background summary built with the configured prompt is as
	// good as a fresh one; applying it is all-or-nothing.
	if !customPrompt && a.applyReadyPrecompress(ctx) {
		return nil
	}

	toolSchemas := a.toolkit.GetToolSchemas()
	reserveTokens := a.reserveTokens(cfg, ctxSize)
	if cfg.CompactStrategy == CompactStrategyFreshWindow {
		reserveTokens = 1 // see compressContextImpl
	}
	msgsToCompress, msgsToReserve := a.splitContextForCompression(reserveTokens, toolSchemas)
	if len(msgsToCompress) == 0 {
		if cfg.CompactStrategy == CompactStrategyFreshWindow {
			return nil
		}
		msgsToCompress, msgsToReserve = a.splitContextForCompression(0, toolSchemas)
		// The reserve-zero split compresses everything; strict mode refuses
		// to fold a call that still awaits its result into the summary.
		if id := openToolCall(msgsToCompress); id != "" {
			return fmt.Errorf("agent: strict compaction: tool call %q still awaits its result", id)
		}
	}
	if len(msgsToCompress) == 0 {
		return nil
	}

	a.mu.Lock()
	systemPrompt := a.systemPrompt
	if len(a.middlewares) > 0 {
		systemPrompt = middleware.ApplySystemPromptPipeline(ctx, a.middlewares, a.name, systemPrompt)
	}
	summary := a.state.Summary
	a.mu.Unlock()

	compressionMsgs := buildCompressionMessages(systemPrompt, summary, msgsToCompress, cfg.CompressionPrompt)
	result, usage, err := model.GenerateStructuredOutputWithUsage(ctx, a.model, compressionMsgs, cfg.SummarySchema)
	a.recordCompressionUsage(usage)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return fmt.Errorf("agent: strict compaction: summary generation: %w", err)
	}
	newSummary, err := formatSummary(cfg.SummaryTemplate, result)
	if err != nil {
		return fmt.Errorf("agent: strict compaction: %w", err)
	}
	// A custom template over blank fields formats to nothing; installing it
	// would replace the history with an empty summary.
	if strings.TrimSpace(newSummary) == "" {
		return errors.New("agent: strict compaction: the summary is empty")
	}
	if budget := a.ownSummaryBudget(cfg, ctxSize); budget > 0 {
		if truncated, did := TruncateToolResult(newSummary, budget); did {
			newSummary = truncated
		}
	}
	if a.offloader != nil {
		if path, offErr := a.offloader.OffloadContent(ctx, newSummary, "compressed_context.txt"); offErr != nil {
			logrus.WithError(offErr).WithField("agent", a.name).Warn("failed to offload compressed context")
		} else {
			newSummary += fmt.Sprintf(
				"\n<system-reminder>The compressed context is offloaded to '%s'.</system-reminder>",
				path,
			)
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	// The split was planned on a snapshot; commit only if the context is
	// still that snapshot (between replies nothing should have changed).
	if err := a.busyLocked("strict compaction"); err != nil {
		return err
	}
	if n := len(a.state.Context); a.state.Summary != summary || n != contextLen || a.state.Context[n-1] != contextLast {
		return errStrictStale
	}
	a.state.Summary = newSummary
	a.state.Context = msgsToReserve
	a.lastModelInputTokens = 0
	a.dropReadyLocked()
	a.compactionVersion++
	if a.readCache != nil {
		cleanReadCacheForReserved(a.readCache, msgsToReserve)
	}
	return nil
}
