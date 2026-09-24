package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

const defaultContextSize = 128000

var defaultSummarySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"task_overview": {
			"type": "string",
			"description": "The user's core request and success criteria. Any clarifications or constraints they specified."
		},
		"current_state": {
			"type": "string",
			"description": "What has been completed so far. Files created, modified, or analyzed (with paths if relevant). Key outputs or artifacts produced."
		},
		"important_discoveries": {
			"type": "string",
			"description": "Technical constraints or requirements uncovered. Decisions made and their rationale. Errors encountered and how they were resolved. What approaches were tried that didn't work (and why)."
		},
		"next_steps": {
			"type": "string",
			"description": "Specific actions needed to complete the task. Any blockers or open questions to resolve. Priority order if multiple steps remain."
		},
		"context_to_preserve": {
			"type": "string",
			"description": "User preferences or style requirements. Domain-specific details that aren't obvious. Any promises made to the user."
		}
	},
	"required": ["task_overview", "current_state", "important_discoveries", "next_steps", "context_to_preserve"]
}`)

const defaultCompressionPrompt = "<system-hint>You have been working on the task described above " +
	"but have not yet completed it. " +
	"Now write a continuation summary that will allow you to resume " +
	"work efficiently in a future context window where the " +
	"conversation history will be replaced with this summary. " +
	"Your summary should be structured, concise, and actionable." +
	"</system-hint>"

const defaultSummaryTemplate = "<system-info>Here is a summary of your previous work\n" +
	"# Task Overview\n" +
	"{task_overview}\n\n" +
	"# Current State\n" +
	"{current_state}\n\n" +
	"# Important Discoveries\n" +
	"{important_discoveries}\n\n" +
	"# Next Steps\n" +
	"{next_steps}\n\n" +
	"# Context to Preserve\n" +
	"{context_to_preserve}" +
	"</system-info>"

// ContextConfig controls when and how the agent compresses its context window.
type ContextConfig struct {
	TriggerRatio      float64         // Token ratio to trigger compression (default 0.8, max 0.9).
	ReserveRatio      float64         // Ratio of tokens to keep as recent context (default 0.1).
	ContextSize       int             // Override context window size (0 = auto-detect from model).
	CompressionPrompt string          // Prompt guiding the model to generate a summary.
	SummaryTemplate   string          // Template with {field} placeholders for the structured summary.
	SummarySchema     json.RawMessage // JSON Schema for structured output (matches SummarySchema fields).
	ToolResultLimit   int             // Max token estimate for individual tool results (default 50000).
	MaxImageNum       int             // Max images kept in context; oldest are replaced by text reminders (0 = unlimited, upstream #2362).

	// AgentDrivenTriggerRatio is the token ratio at which the model-invoked
	// compress_context tool may compress (upstream #2143). It has to be lower
	// than TriggerRatio: automatic compression already fires at TriggerRatio,
	// so with the same number the tool could never do anything the agent had
	// not already done. 0 (the default) means TriggerRatio/2; values above
	// TriggerRatio are clamped down to it.
	AgentDrivenTriggerRatio float64
}

// agentDrivenTriggerRatio resolves the effective threshold for a
// model-initiated compression. See ContextConfig.AgentDrivenTriggerRatio.
func agentDrivenTriggerRatio(cfg *ContextConfig) float64 {
	r := cfg.AgentDrivenTriggerRatio
	if r <= 0 {
		r = cfg.TriggerRatio / 2
	}
	if r <= 0 {
		r = 0.4 // TriggerRatio was itself unset; use a sane standalone value
	}
	// Clamp below the automatic threshold, but only when that threshold is
	// actually configured: clamping against an unset (zero) TriggerRatio
	// would collapse the ratio to 0 and make the tool never fire.
	if cfg.TriggerRatio > 0 && r > cfg.TriggerRatio {
		r = cfg.TriggerRatio
	}
	return r
}

func (c *ContextConfig) withDefaults() ContextConfig {
	cfg := *c
	// Upstream #2396: 0.9 itself is a legal trigger ratio; only values
	// above it (or <= 0) fall back to the default.
	if cfg.TriggerRatio <= 0 || cfg.TriggerRatio > 0.9 {
		cfg.TriggerRatio = 0.8
	}
	if cfg.ReserveRatio <= 0 || cfg.ReserveRatio >= 0.9 {
		cfg.ReserveRatio = 0.1
	}
	if cfg.CompressionPrompt == "" {
		cfg.CompressionPrompt = defaultCompressionPrompt
	}
	if cfg.SummaryTemplate == "" {
		cfg.SummaryTemplate = defaultSummaryTemplate
	}
	if cfg.SummarySchema == nil {
		cfg.SummarySchema = defaultSummarySchema
	}
	if cfg.ToolResultLimit <= 0 {
		cfg.ToolResultLimit = 50000
	}
	return cfg
}

// compressContext checks if the context exceeds the token threshold and, if so,
// generates a structured summary of old messages using the model.
// It runs through the OnCompressContext middleware chain if middlewares are configured.
func (a *UnifiedAgent) compressContext(ctx context.Context) error {
	_, err := a.compressContextWithRatio(ctx, 0)
	return err
}

// compressContextWithRatio runs the compression chain and reports whether the
// context was actually summarized. triggerOverride replaces the configured
// TriggerRatio when > 0; the agent-driven compress_context tool uses it to
// lower the bar so the model can compress before the automatic threshold.
//
// "Actually summarized" is observed from state (a new non-empty Summary, or a
// changed context length) rather than from the handler's return value, so the
// middleware CompressHandler API stays untouched.
func (a *UnifiedAgent) compressContextWithRatio(ctx context.Context, triggerOverride float64) (bool, error) {
	if a.contextCfg == nil {
		return false, nil
	}

	a.mu.Lock()
	prevSummary := a.state.Summary
	prevLen := len(a.state.Context)
	a.mu.Unlock()

	base := *a.contextCfg
	if triggerOverride > 0 {
		base.TriggerRatio = triggerOverride
	}

	var err error
	if len(a.middlewares) == 0 {
		cfgCopy := base
		err = a.compressContextImpl(ctx, &cfgCopy)
	} else {
		core := func(ctx context.Context, input middleware.CompressInput) error {
			cfg := base
			cfg.TriggerRatio = input.TriggerRatio
			cfg.ReserveRatio = input.ReserveRatio
			return a.compressContextImpl(ctx, &cfg)
		}
		chain := middleware.BuildCompressChain(a.middlewares, core)
		err = chain(ctx, middleware.CompressInput{
			AgentName:    a.name,
			TriggerRatio: base.TriggerRatio,
			ReserveRatio: base.ReserveRatio,
		})
	}

	a.mu.Lock()
	compressed := (a.state.Summary != prevSummary && a.state.Summary != "") ||
		len(a.state.Context) != prevLen
	a.mu.Unlock()
	return compressed, err
}

// compressContextForTool is the seam behind the compress_context tool
// (upstream #2143). It compresses at the lower agent-driven threshold and
// reports honestly whether anything happened, so the tool never tells the model
// that details were summarized away when the context is unchanged.
func (a *UnifiedAgent) compressContextForTool(ctx context.Context) (tool.CompressionResult, error) {
	if a.contextCfg == nil {
		return tool.CompressionResult{Detail: "Context compression is not configured for this agent."}, nil
	}
	compressed, err := a.compressContextWithRatio(ctx, agentDrivenTriggerRatio(a.contextCfg))
	if err != nil {
		return tool.CompressionResult{}, err
	}
	return tool.CompressionResult{Compressed: compressed}, nil
}

func (a *UnifiedAgent) compressContextImpl(ctx context.Context, cfg *ContextConfig) (resultErr error) {
	ctxSize := cfg.ContextSize
	if ctxSize == 0 {
		ctxSize = model.ResolveContextSize(a.model, defaultContextSize)
	}

	// Upstream #2362: limit the images carried in context first, so the
	// token estimate below reflects the images that actually remain.
	if cfg.MaxImageNum > 0 {
		a.limitContextImages(cfg.MaxImageNum)
	}

	modelMsgs := a.prepareModelInput(ctx)
	toolSchemas := a.toolkit.GetToolSchemas()
	estimatedTokens := a.model.CountTokens(modelMsgs, toolSchemas)

	threshold := int(float64(ctxSize) * cfg.TriggerRatio)
	if estimatedTokens < threshold {
		return nil
	}

	a.mu.Lock()
	contextLen := len(a.state.Context)
	a.mu.Unlock()

	if contextLen == 0 {
		return fmt.Errorf("agent %s: system prompt (and summary) exceed compression threshold (%d tokens), cannot compress", a.name, threshold)
	}

	logrus.WithFields(logrus.Fields{
		"agent":     a.name,
		"tokens":    estimatedTokens,
		"threshold": threshold,
	}).Info("context compression triggered")

	reserveTokens := int(float64(ctxSize) * cfg.ReserveRatio)
	msgsToCompress, msgsToReserve := a.splitContextForCompression(reserveTokens, toolSchemas)

	if len(msgsToCompress) == 0 {
		logrus.WithField("agent", a.name).Warn("reserve ratio too large, falling back to reserve_ratio=0")
		msgsToCompress, msgsToReserve = a.splitContextForCompression(0, toolSchemas)
	}

	a.mu.Lock()
	systemPrompt := a.systemPrompt
	if len(a.middlewares) > 0 {
		systemPrompt = middleware.ApplySystemPromptPipeline(ctx, a.middlewares, a.name, systemPrompt)
	}
	summary := a.state.Summary
	a.mu.Unlock()

	compressionMsgs := buildCompressionMessages(systemPrompt, summary, msgsToCompress, cfg.CompressionPrompt)

	compressionToolSchema := []model.ToolSchema{
		{
			Type: "function",
			Function: model.ToolFunction{
				Name:        "generate_structured_output",
				Description: "Call this function to generate structured output required by the user.",
				Parameters:  cfg.SummarySchema,
			},
		},
	}
	compTokens := a.model.CountTokens(compressionMsgs, compressionToolSchema)
	contextOverflow := compTokens > ctxSize

	// Upstream #2433: compression calls burn tokens; accumulate their usage
	// so the reply loop can account it (budgets, cost tracking, reply msg).
	ctx = inference.WithPurpose(a.inferenceContext(ctx), "summary")
	if managed, ok := a.model.(model.ManagedChatModel); ok {
		var finish func(error)
		ctx, finish = managed.ManagedDeployment().Start(ctx, "summary")
		defer func() { finish(resultErr) }()
	}
	result, compUsage, err := model.GenerateStructuredOutputWithUsage(ctx, a.model, compressionMsgs, cfg.SummarySchema)
	a.recordCompressionUsage(compUsage)
	if err != nil {
		if contextOverflow {
			logrus.WithField("agent", a.name).Warn("compression context overflow, removing oldest messages and retrying")
			var retryUsage *model.ChatUsage
			result, retryUsage, err = a.retryCompressWithFewer(ctx, compressionMsgs, msgsToCompress, cfg, ctxSize, compressionToolSchema)
			a.recordCompressionUsage(retryUsage)
		}
		if err != nil {
			// Upstream #2140: a failed summary must not leave the context
			// wedged above the threshold. Fall back to truncating to the
			// reserve set while keeping the previous summary.
			logrus.WithError(err).WithField("agent", a.name).
				Warn("summary generation failed; falling back to truncation with previous summary")
			a.fallbackTruncateKeepSummary(ctx, msgsToCompress, msgsToReserve)
			return nil
		}
	}

	newSummary, err := formatSummary(cfg.SummaryTemplate, result)
	if err != nil {
		// Unusable summary: same fallback as a generation failure (#2140).
		logrus.WithError(err).WithField("agent", a.name).
			Warn("summary formatting failed; falling back to truncation with previous summary")
		a.fallbackTruncateKeepSummary(ctx, msgsToCompress, msgsToReserve)
		return nil
	}

	// Offload the compressed context to workspace if offloader is set
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
	a.state.Summary = newSummary
	a.state.Context = msgsToReserve
	a.mu.Unlock()

	if a.readCache != nil {
		cleanReadCacheForReserved(a.readCache, msgsToReserve)
	}

	logrus.WithField("agent", a.name).Info("context compression finished")
	return nil
}

// defaultTruncationNotice replaces a missing summary when compression falls
// back to truncation, so the model knows earlier history was dropped
// (upstream #2140).
const defaultTruncationNotice = "<system-info>Some earlier messages were truncated for limited context.</system-info>"

// fallbackTruncateKeepSummary resolves a failed compression by truncating
// the context to the reserve set while keeping the previous summary
// (upstream #2140), so the reply can proceed instead of staying wedged
// above the compression threshold. The dropped messages are offloaded when
// an offloader is configured (with a deduplicated reminder), and an empty
// summary is replaced by a truncation notice.
func (a *UnifiedAgent) fallbackTruncateKeepSummary(ctx context.Context, msgsToCompress, msgsToReserve []*message.Msg) {
	a.mu.Lock()
	summary := a.state.Summary
	a.mu.Unlock()

	if summary == "" {
		summary = defaultTruncationNotice
	}

	// Offload the dropped messages regardless of whether a summary existed
	// (Python offloads unconditionally after the notice substitution).
	if a.offloader != nil && len(msgsToCompress) > 0 {
		if content, mErr := json.Marshal(msgsToCompress); mErr == nil {
			if path, offErr := a.offloader.OffloadContent(ctx, string(content), "compressed_context_fallback.json"); offErr != nil {
				logrus.WithError(offErr).WithField("agent", a.name).Warn("failed to offload truncated context")
			} else {
				reminder := fmt.Sprintf(
					"\n<system-reminder>The truncated context is offloaded to '%s'.</system-reminder>",
					path,
				)
				// Avoid duplicating the reminder across repeated fallbacks.
				if !strings.Contains(summary, reminder) {
					summary += reminder
				}
			}
		}
	}

	a.mu.Lock()
	a.state.Summary = summary
	a.state.Context = msgsToReserve
	a.mu.Unlock()
	if a.readCache != nil {
		cleanReadCacheForReserved(a.readCache, msgsToReserve)
	}
}

func (a *UnifiedAgent) retryCompressWithFewer(
	ctx context.Context,
	baseMsgs []*message.Msg,
	msgsToCompress []*message.Msg,
	cfg *ContextConfig,
	ctxSize int,
	compressionToolSchema []model.ToolSchema,
) (json.RawMessage, *model.ChatUsage, error) {
	a.mu.Lock()
	systemPrompt := a.systemPrompt
	if len(a.middlewares) > 0 {
		systemPrompt = middleware.ApplySystemPromptPipeline(ctx, a.middlewares, a.name, systemPrompt)
	}
	summary := a.state.Summary
	a.mu.Unlock()

	triggerThreshold := int(float64(ctxSize) * cfg.TriggerRatio)

	for i := 1; i <= len(msgsToCompress); i++ {
		msgs := buildCompressionMessages(systemPrompt, summary, msgsToCompress[i:], cfg.CompressionPrompt)
		tokens := a.model.CountTokens(msgs, compressionToolSchema)
		if tokens < triggerThreshold {
			return model.GenerateStructuredOutputWithUsage(ctx, a.model, msgs, cfg.SummarySchema)
		}
	}
	return nil, nil, fmt.Errorf("cannot reduce context below threshold")
}

// splitContextForCompression splits state.Context into messages to compress and messages to reserve.
// It walks backward from the end, accumulating messages until the reserved portion
// reaches the token budget, keeping tool call/result pairs together.
func (a *UnifiedAgent) splitContextForCompression(reserveTokenBudget int, tools []model.ToolSchema) ([]*message.Msg, []*message.Msg) {
	a.mu.Lock()
	ctxMsgs := make([]*message.Msg, len(a.state.Context))
	copy(ctxMsgs, a.state.Context)
	systemPrompt := a.systemPrompt
	summary := a.state.Summary
	a.mu.Unlock()

	baseMsgs := []*message.Msg{message.SystemMsg(a.name, systemPrompt)}
	if summary != "" {
		baseMsgs = append(baseMsgs, message.UserMsg(a.name, "[Previous context summary]: "+summary))
	}

	if reserveTokenBudget <= 0 {
		// Emergency path (reserve budget disabled, or the retry after a
		// split that produced nothing): everything is compressed, including
		// any call still in flight. At this point the context is already
		// over the window, so losing an in-flight pair beats an unusable
		// agent.
		return copyMsgs(ctxMsgs), nil
	}

	splitIdx := len(ctxMsgs)
	for i := len(ctxMsgs) - 1; i >= 0; i-- {
		candidate := make([]*message.Msg, len(baseMsgs))
		copy(candidate, baseMsgs)
		candidate = append(candidate, ctxMsgs[i:]...)
		tokens := a.model.CountTokens(candidate, tools)
		if tokens >= reserveTokenBudget {
			splitIdx = i + 1
			break
		}
		if i == 0 {
			return nil, copyMsgs(ctxMsgs)
		}
	}

	if splitIdx >= len(ctxMsgs) {
		splitIdx = adjustSplitForToolPairs(ctxMsgs, len(ctxMsgs)-1)
	} else {
		splitIdx = adjustSplitForToolPairs(ctxMsgs, splitIdx)
	}
	// Applied last, and it iterates: adjustSplitForToolPairs only pushes the
	// split FORWARD, which can swallow a call that is still in flight and can
	// also leave a result in the reserved half whose call sits in the
	// compressed half. Pulling the split back repairs both.
	splitIdx = pullSplitBackForToolPairs(ctxMsgs, splitIdx)

	return copyMsgs(ctxMsgs[:splitIdx]), copyMsgs(ctxMsgs[splitIdx:])
}

// adjustSplitForToolPairs ensures tool result messages aren't separated from their
// corresponding tool call messages. It pushes the split point forward if needed
// so that orphan tool results (whose tool call is in the compressed portion)
// move into the compressed portion as well.
//
// This is the forward half of the repair; pullSplitBackForToolPairs runs after
// it and has the last word, because pushing forward can strand an in-flight
// call. See that function for why backward dominates.
func adjustSplitForToolPairs(msgs []*message.Msg, splitIdx int) int {
	callIDs := make(map[string]bool)
	resultPositions := make(map[string]int)

	for i := splitIdx; i < len(msgs); i++ {
		for _, b := range msgs[i].GetContentBlocks(message.ContentBlockToolCall) {
			if tc, ok := b.(message.ToolCallBlock); ok {
				callIDs[tc.ID] = true
			}
		}
		for _, b := range msgs[i].GetContentBlocks(message.ContentBlockToolResult) {
			if tr, ok := b.(message.ToolResultBlock); ok {
				resultPositions[tr.ID] = i
			}
		}
	}

	maxOrphanIdx := -1
	for id, pos := range resultPositions {
		if !callIDs[id] && pos > maxOrphanIdx {
			maxOrphanIdx = pos
		}
	}

	if maxOrphanIdx < 0 {
		return splitIdx
	}
	return maxOrphanIdx + 1
}

// pullSplitBackForToolPairs moves the split point BACKWARD until two invariants
// hold, iterating to a fixpoint (each pass only lowers the index, so it
// terminates in at most len(msgs) passes):
//
//  1. No tool call that is still waiting for its result may sit in the
//     compressed half. This matters when the model calls compress_context from
//     inside the acting loop: the assistant message holding the batch's calls
//     is already in the context, and summarizing it away leaves the results
//     that land moments later orphaned (upstream #2143 tracks these as
//     unfinished_tool_call_ids).
//  2. No call may end up on the opposite side from its own result. Providers
//     reject a tool result whose call is not in the request.
//
// Backward is the only safe direction here. adjustSplitForToolPairs repairs a
// split pair by pushing the split forward so the RESULT joins its call in the
// compressed half; that is right when the call has already left the context
// entirely, but when the call is still present it can drag an in-flight call
// into the summary and can expose further split pairs one index later. Pulling
// the CALL into the reserved half fixes the same pair without either side
// effect, so it dominates. The two are composed forward-then-backward, and the
// backward pass has the last word.
func pullSplitBackForToolPairs(msgs []*message.Msg, splitIdx int) int {
	if splitIdx <= 0 || splitIdx > len(msgs) {
		return splitIdx
	}

	callIdx := make(map[string]int)
	resultIdx := make(map[string]int)
	for i, m := range msgs {
		if m == nil {
			continue
		}
		for _, b := range m.Content {
			switch blk := b.(type) {
			case message.ToolCallBlock:
				if _, seen := callIdx[blk.ID]; !seen {
					callIdx[blk.ID] = i
				}
			case message.ToolResultBlock:
				if _, seen := resultIdx[blk.ID]; !seen {
					resultIdx[blk.ID] = i
				}
			}
		}
	}

	for {
		limit := splitIdx
		for id, c := range callIdx {
			if c >= limit {
				continue // already reserved
			}
			r, hasResult := resultIdx[id]
			if !hasResult {
				// In flight: must not be summarized away.
				limit = c
				continue
			}
			if r >= limit {
				// Split pair: pull the call into the reserved half so both
				// sides of the exchange travel together.
				limit = c
			}
		}
		if limit == splitIdx {
			return splitIdx
		}
		splitIdx = limit
	}
}

// TruncateToolResult truncates a tool result string if it exceeds the token limit.
// Returns the (possibly truncated) text and whether truncation occurred.
func TruncateToolResult(text string, tokenLimit int) (string, bool) {
	estimatedTokens := len(text) / 4
	if estimatedTokens <= tokenLimit {
		return text, false
	}
	charLimit := tokenLimit * 4
	if charLimit >= len(text) {
		return text, false
	}
	return text[:charLimit] + "\n<<<TRUNCATED>>>", true
}

// SplitToolResultForCompression performs token-aware binary search to split
// a tool result into (reserved, offloaded) portions. It uses the model's
// CountTokens to find the largest prefix that fits under tokenLimit.
// Falls back to character-based truncation if no model is available.
func SplitToolResultForCompression(text string, tokenLimit int, counter TokenCounter) (reserved, offloaded string, wasSplit bool) {
	if counter == nil {
		r, split := TruncateToolResult(text, tokenLimit)
		if split {
			return r, text[len(r)-len("\n<<<TRUNCATED>>>"):], true
		}
		return text, "", false
	}

	probe := []*message.Msg{message.UserMsg("_", text)}
	tokens := counter.CountTokens(probe, nil)
	if tokens <= tokenLimit {
		return text, "", false
	}

	// Binary search for the split point
	lo, hi := 0, len(text)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		probe = []*message.Msg{message.UserMsg("_", text[:mid])}
		t := counter.CountTokens(probe, nil)
		if t <= tokenLimit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}

	if lo == 0 {
		lo = 1
	}
	return text[:lo] + "\n<<<TRUNCATED>>>", text[lo:], true
}

// TokenCounter is the subset of model.ChatModel needed for token counting.
type TokenCounter interface {
	CountTokens(msgs []*message.Msg, tools []model.ToolSchema) int
}

// TruncateToolResultBlocks truncates individual blocks within a multi-block tool result.
// Text blocks are truncated by character limit. Data blocks with Base64Source
// have their data replaced with a placeholder.
func TruncateToolResultBlocks(blocks []message.ContentBlock, tokenLimit int) []message.ContentBlock {
	totalTokens := 0
	for _, b := range blocks {
		switch blk := b.(type) {
		case message.TextBlock:
			totalTokens += len(blk.Text) / 4
		case message.DataBlock:
			if src, ok := blk.Source.(message.Base64Source); ok {
				totalTokens += len(src.Data) * 3 / 16 // base64 → raw → tokens
			}
		}
	}

	if totalTokens <= tokenLimit {
		return blocks
	}

	result := make([]message.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch blk := b.(type) {
		case message.TextBlock:
			charLimit := tokenLimit * 4
			if len(blk.Text) > charLimit {
				blk.Text = blk.Text[:charLimit] + "\n<<<TRUNCATED>>>"
			}
			result = append(result, blk)
		case message.DataBlock:
			// Replace large base64 data with a placeholder
			if src, ok := blk.Source.(message.Base64Source); ok && len(src.Data) > 1000 {
				blk.Source = message.Base64Source{
					Type:      "base64",
					Data:      "",
					MediaType: src.MediaType,
				}
				result = append(result, blk)
			} else {
				result = append(result, blk)
			}
		default:
			result = append(result, b)
		}
	}
	return result
}

// splitMessageAtBlock splits a message into two messages at the given block index.
// The first message contains blocks [0, blockIdx), the second [blockIdx, end).
func splitMessageAtBlock(msg *message.Msg, blockIdx int) (*message.Msg, *message.Msg) {
	if blockIdx <= 0 || blockIdx >= len(msg.Content) {
		return msg, nil
	}
	first := *msg
	first.Content = make([]message.ContentBlock, blockIdx)
	copy(first.Content, msg.Content[:blockIdx])

	second := *msg
	second.Content = make([]message.ContentBlock, len(msg.Content)-blockIdx)
	copy(second.Content, msg.Content[blockIdx:])

	return &first, &second
}

func buildCompressionMessages(systemPrompt, summary string, msgsToCompress []*message.Msg, compressionPrompt string) []*message.Msg {
	msgs := make([]*message.Msg, 0, len(msgsToCompress)+3)
	msgs = append(msgs, message.SystemMsg("system", systemPrompt))
	if summary != "" {
		msgs = append(msgs, message.UserMsg("user", summary))
	}
	msgs = append(msgs, msgsToCompress...)
	msgs = append(msgs, message.UserMsg("user", compressionPrompt))
	return msgs
}

func formatSummary(template string, structuredOutput json.RawMessage) (string, error) {
	var fields map[string]string
	if err := json.Unmarshal(structuredOutput, &fields); err != nil {
		return "", fmt.Errorf("unmarshal structured output: %w", err)
	}

	result := template
	for key, value := range fields {
		result = strings.ReplaceAll(result, "{"+key+"}", value)
	}
	return result, nil
}

func copyMsgs(msgs []*message.Msg) []*message.Msg {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]*message.Msg, len(msgs))
	copy(out, msgs)
	return out
}

// cleanReadCacheForReserved drops cached files not referenced by Read tool calls
// in the reserved messages.
func cleanReadCacheForReserved(rc *tool.ReadCache, reservedMsgs []*message.Msg) {
	keepPaths := make(map[string]bool)

	for _, m := range reservedMsgs {
		for _, b := range m.GetContentBlocks(message.ContentBlockToolCall) {
			tc, ok := b.(message.ToolCallBlock)
			if !ok || tc.Name != "Read" {
				continue
			}
			var args struct {
				FilePath string `json:"file_path"`
			}
			if err := json.Unmarshal([]byte(tc.Input), &args); err == nil && args.FilePath != "" {
				keepPaths[args.FilePath] = true
			}
		}
	}

	rc.CleanFileCache(keepPaths)
}

// limitContextImages caps the number of image data blocks carried in the
// agent context (upstream #2362). The oldest images beyond the cap are
// replaced by a system-reminder text block; a URL-backed image keeps a
// pointer to its URL so the model can refer back to it. Images nested in
// tool results or hint blocks become TextBlocks (those containers only
// accept text/data blocks); top-level images in non-user messages become
// HintBlocks, matching Python's replacement rules.
func (a *UnifiedAgent) limitContextImages(maxImages int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	type imageRef struct {
		msgIdx    int
		blockIdx  int
		nested    int // 0 = top-level, 1 = tool-result output list, 2 = hint list
		nestedIdx int
		block     message.DataBlock
	}
	isImage := func(b message.ContentBlock) (message.DataBlock, bool) {
		db, ok := b.(message.DataBlock)
		if !ok {
			return db, false
		}
		return db, strings.HasPrefix(db.GetMediaType(), "image/")
	}

	var images []imageRef
	for mi, msg := range a.state.Context {
		if msg == nil {
			continue
		}
		for bi, blk := range msg.Content {
			if db, ok := isImage(blk); ok {
				images = append(images, imageRef{msgIdx: mi, blockIdx: bi, block: db})
				continue
			}
			switch nb := blk.(type) {
			case message.ToolResultBlock:
				if list, ok := nb.Output.([]message.ContentBlock); ok {
					for j, sub := range list {
						if db, ok := isImage(sub); ok {
							images = append(images, imageRef{msgIdx: mi, blockIdx: bi, nested: 1, nestedIdx: j, block: db})
						}
					}
				}
			case message.HintBlock:
				if list, ok := nb.Hint.([]message.ContentBlock); ok {
					for j, sub := range list {
						if db, ok := isImage(sub); ok {
							images = append(images, imageRef{msgIdx: mi, blockIdx: bi, nested: 2, nestedIdx: j, block: db})
						}
					}
				}
			}
		}
	}

	nExceed := len(images) - maxImages
	if nExceed <= 0 {
		return
	}
	logrus.WithFields(logrus.Fields{
		"agent":  a.name,
		"images": len(images),
		"limit":  maxImages,
	}).Infof("context image count exceeds limit, removing the oldest %d image(s)", nExceed)

	// Copy-on-write. a.state.Context shares its *Msg pointers with snapshots
	// handed out earlier under a.mu and then read after unlocking
	// (prepareModelInput, splitContextForCompression, checkpoint saves, HTTP
	// history reads). Writing msg.Content[i] in place races with those
	// readers, so each touched message is cloned first and the clone is
	// published back into the context; readers keep seeing the old, now
	// immutable value. The same applies one level down: a cloned message's
	// ToolResultBlock.Output / HintBlock.Hint still share their backing
	// array, so those lists are cloned too.
	clonedMsgs := make(map[int]*message.Msg)
	clonedLists := make(map[[2]int][]message.ContentBlock)
	cloneMsg := func(mi int) *message.Msg {
		if c, ok := clonedMsgs[mi]; ok {
			return c
		}
		orig := a.state.Context[mi]
		cp := *orig
		cp.Content = make([]message.ContentBlock, len(orig.Content))
		copy(cp.Content, orig.Content)
		clonedMsgs[mi] = &cp
		return &cp
	}
	cloneList := func(mi, bi int, list []message.ContentBlock) []message.ContentBlock {
		key := [2]int{mi, bi}
		if c, ok := clonedLists[key]; ok {
			return c
		}
		cp := make([]message.ContentBlock, len(list))
		copy(cp, list)
		clonedLists[key] = cp
		return cp
	}

	for _, ref := range images[:nExceed] {
		msg := cloneMsg(ref.msgIdx)
		url := ""
		if src, ok := ref.block.Source.(message.URLSource); ok {
			url = src.URL
		}
		name := ""
		if ref.block.Name != "" {
			name = "named '" + ref.block.Name + "' "
		}
		var text string
		if url != "" {
			text = "<system-reminder>The image " + name + "is offloaded into " + url + ", you can refer to it when needed.</system-reminder>"
		} else {
			text = "<system-reminder>The image " + name + "is removed to free up context space.</system-reminder>"
		}
		switch ref.nested {
		case 0:
			if msg.Role != message.RoleUser {
				msg.Content[ref.blockIdx] = message.HintBlock{Type: "hint", Hint: text}
			} else {
				msg.Content[ref.blockIdx] = message.TextBlock{Type: "text", Text: text}
			}
		case 1:
			tb, ok := msg.Content[ref.blockIdx].(message.ToolResultBlock)
			if !ok {
				continue
			}
			list, ok := tb.Output.([]message.ContentBlock)
			if !ok || ref.nestedIdx >= len(list) {
				continue
			}
			list = cloneList(ref.msgIdx, ref.blockIdx, list)
			list[ref.nestedIdx] = message.TextBlock{Type: "text", Text: text}
			tb.Output = list
			msg.Content[ref.blockIdx] = tb
		case 2:
			hb, ok := msg.Content[ref.blockIdx].(message.HintBlock)
			if !ok {
				continue
			}
			list, ok := hb.Hint.([]message.ContentBlock)
			if !ok || ref.nestedIdx >= len(list) {
				continue
			}
			list = cloneList(ref.msgIdx, ref.blockIdx, list)
			list[ref.nestedIdx] = message.TextBlock{Type: "text", Text: text}
			hb.Hint = list
			msg.Content[ref.blockIdx] = hb
		}
	}

	// Publish the clones. Doing this after the loop (rather than per
	// reference) keeps a.state.Context consistent for the whole scan.
	for mi, cp := range clonedMsgs {
		a.state.Context[mi] = cp
	}
}

// recordCompressionUsage accumulates token usage burned by compression model
// calls (upstream #2433). The reply loop drains it via takeCompressionUsage
// and surfaces it as a model-call-end event.
func (a *UnifiedAgent) recordCompressionUsage(u *model.ChatUsage) {
	if u == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingCompressionUsage == nil {
		cp := *u
		a.pendingCompressionUsage = &cp
		return
	}
	p := a.pendingCompressionUsage
	p.InputTokens += u.InputTokens
	p.OutputTokens += u.OutputTokens
	p.CacheCreationInputTokens += u.CacheCreationInputTokens
	p.CacheInputTokens += u.CacheInputTokens
}

// takeCompressionUsage returns and clears the accumulated compression usage.
func (a *UnifiedAgent) takeCompressionUsage() *model.ChatUsage {
	a.mu.Lock()
	defer a.mu.Unlock()
	u := a.pendingCompressionUsage
	a.pendingCompressionUsage = nil
	return u
}
