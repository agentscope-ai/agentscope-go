package runtime

import (
	"context"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
)

// TurnConfig holds the dependencies for a single Turn execution.
type TurnConfig struct {
	Loop   *loop.Loop
	Hooks  *SessionHookManager
	Budget *BudgetTracker
}

// Turn represents a single agent turn: one user input producing one stream of
// events via the underlying Loop. Each Turn has a unique ID and runs
// pre/post hooks and budget checks around the loop execution.
type Turn struct {
	id  string
	cfg TurnConfig
}

// NewTurn creates a new Turn with a unique ID and the given configuration.
func NewTurn(cfg TurnConfig) *Turn {
	return &Turn{
		id:  agentscope.GenerateID(),
		cfg: cfg,
	}
}

// ID returns the unique identifier for this turn.
func (t *Turn) ID() string { return t.id }

// Run starts the turn in a background goroutine and returns an event channel.
// The channel is closed when the turn finishes. Callers should range over the
// channel to receive all events.
func (t *Turn) Run(ctx context.Context, input string) <-chan event.Event {
	out := make(chan event.Event, 64)
	go t.run(ctx, input, out)
	return out
}

func (t *Turn) run(ctx context.Context, input string, out chan<- event.Event) {
	defer close(out)

	// Fire pre-turn hook.
	if t.cfg.Hooks != nil {
		if err := t.cfg.Hooks.Fire(HookPreTurn, map[string]any{"turn_id": t.id, "input": input}); err != nil {
			emitEvent(ctx, out, event.NewCustomEvent("", "turn.hook_error", map[string]any{"error": err, "hook": "pre_turn"}))
			return
		}
	}

	// Check budget.
	if t.cfg.Budget != nil {
		if err := t.cfg.Budget.AddTurn(); err != nil {
			emitEvent(ctx, out, event.NewCustomEvent("", "turn.budget_exceeded", map[string]any{"error": err}))
			return
		}
	}

	// Guard against nil loop.
	if t.cfg.Loop == nil {
		emitEvent(ctx, out, event.NewCustomEvent("", "turn.error", map[string]any{"error": "loop is nil"}))
		return
	}

	// Forward all events from the loop, enforcing the token/duration budget as
	// events arrive. A cancelable child context lets us stop the loop the moment
	// the budget is exceeded (previously only MaxTurns was enforced; MaxTokens
	// and MaxDuration were dead config on this path).
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	loopEvents := t.cfg.Loop.Run(runCtx, input)
	for ev := range loopEvents {
		emitEvent(ctx, out, ev)
		if t.cfg.Budget == nil {
			continue
		}
		if mce, ok := ev.(event.ModelCallEndEvent); ok {
			// Count every token dimension the model reported, including prompt-cache
			// creation/read tokens (surfaced on the event), so the budget reflects
			// true consumption.
			_ = t.cfg.Budget.AddTokens(mce.InputTokens + mce.OutputTokens + mce.CacheCreationTokens + mce.CacheReadTokens)
		}
		if t.cfg.Budget.Exceeded() {
			emitEvent(ctx, out, event.NewCustomEvent("", "turn.budget_exceeded",
				map[string]any{"error": "budget exceeded (tokens or duration)"}))
			cancel()
			// Wait for the loop to stop mutating history before the session
			// persists it or starts the next turn.
			for range loopEvents {
			}
			break
		}
	}

	// Fire post-turn hook.
	if t.cfg.Hooks != nil {
		if err := t.cfg.Hooks.Fire(HookPostTurn, map[string]any{"turn_id": t.id}); err != nil {
			emitEvent(ctx, out, event.NewCustomEvent("", "turn.hook_error", map[string]any{"error": err, "hook": "post_turn"}))
		}
	}
}
