package agent

import (
	"context"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// Per-reply usage accounting for the turn_stats custom event (codex
// token-count parity). The accumulator lives on the agent for the duration
// of one reply; reactRound records into it, replyLoop emits it alongside the
// terminal ReplyEndEvent. Hosts use it for cost display, budget tracking and
// context-fill signals without re-deriving usage from ModelCallEndEvents.

type turnStats struct {
	start        time.Time
	modelCalls   int
	toolCalls    int
	iterations   int
	inputTokens  int
	outputTokens int
	cacheCreate  int
	cacheRead    int
}

// beginTurnStats resets the per-reply accumulator. Called once at the start
// of replyLoop; a swallowed ReplyEnd keeps accumulating into the next round
// (rounds of one reply are one turn from the host's perspective).
func (a *UnifiedAgent) beginTurnStats() {
	a.mu.Lock()
	a.stats = &turnStats{start: time.Now()}
	a.mu.Unlock()
}

func (a *UnifiedAgent) recordModelCallUsage(u *model.ChatUsage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stats == nil {
		return
	}
	a.stats.modelCalls++
	if u != nil {
		a.stats.inputTokens += u.InputTokens
		a.stats.outputTokens += u.OutputTokens
		a.stats.cacheCreate += u.CacheCreationInputTokens
		a.stats.cacheRead += u.CacheInputTokens
	}
}

func (a *UnifiedAgent) recordToolCalls(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stats != nil {
		a.stats.toolCalls += n
	}
}

func (a *UnifiedAgent) recordIteration(iter int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stats != nil && iter+1 > a.stats.iterations {
		a.stats.iterations = iter + 1
	}
}

// emitTurnStats emits the turn_stats custom event with the accumulated
// per-reply usage. It accompanies the terminal ReplyEndEvent: immediately
// before it on direct terminal paths, and right after the delivered end
// event on the swallowed-round path (where escaping is only known via the
// verdict). last_input_tokens carries the provider-reported context fill of
// the final model call so hosts can render "tokens left" cheaply.
//
// Compression usage the Reason-state drain did not pick up — a background
// pre-compression may have finished during the final model call — is
// drained here so the reply that caused it accounts for it: recorded into
// the stats and surfaced as a model-call-end event first (upstream #2433
// contract). On the swallowed-round path that event lands after the
// delivered ReplyEndEvent, same as the turn_stats event itself.
func (a *UnifiedAgent) emitTurnStats(ctx context.Context, ch chan<- event.Event, replyID string) {
	if cu := a.takeCompressionUsage(); cu != nil {
		a.recordModelCallUsage(cu)
		emit(ctx, ch, event.NewModelCallEndEventWithCache(replyID,
			cu.InputTokens, cu.OutputTokens,
			cu.CacheCreationInputTokens, cu.CacheInputTokens))
	}
	a.mu.Lock()
	stats := a.stats
	lastInput := a.lastModelInputTokens
	a.mu.Unlock()
	if stats == nil {
		return
	}
	emit(ctx, ch, event.NewCustomEvent(replyID, "turn_stats", map[string]any{
		"model_calls":                 stats.modelCalls,
		"tool_calls":                  stats.toolCalls,
		"iterations":                  stats.iterations,
		"duration_ms":                 time.Since(stats.start).Milliseconds(),
		"input_tokens":                stats.inputTokens,
		"output_tokens":               stats.outputTokens,
		"cache_creation_input_tokens": stats.cacheCreate,
		"cache_read_input_tokens":     stats.cacheRead,
		"last_input_tokens":           lastInput,
	}))
}
