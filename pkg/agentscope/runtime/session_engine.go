package runtime

import (
	"context"
	"sync"
	"time"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
)

// SessionEngineConfig holds the configuration for creating a SessionEngine.
type SessionEngineConfig struct {
	LoopOptions []loop.Option
	Budget      Budget
	Store       SessionStore
}

// SessionEngine orchestrates a multi-turn session. It wires together a Loop,
// BudgetTracker, SessionHookManager, and SessionStore to manage the full
// lifecycle of a user session.
type SessionEngine struct {
	mu             sync.RWMutex
	turnMu         sync.Mutex
	id             string
	loop           *loop.Loop
	contextManager loop.ContextManager
	budget         *BudgetTracker
	hooks          *SessionHookManager
	agents         *AgentManager
	store          SessionStore
	cancelFunc     context.CancelFunc
	state          *SessionState
}

// NewSessionEngine creates a new SessionEngine with a unique ID and the given
// configuration. If no Store is provided, an InMemorySessionStore is used.
func NewSessionEngine(cfg SessionEngineConfig) *SessionEngine {
	id := agentscope.GenerateID()
	var store SessionStore
	if cfg.Store != nil {
		store = cfg.Store
	} else {
		store = NewInMemorySessionStore()
	}

	bt := NewBudgetTracker(cfg.Budget)
	hooks := NewSessionHookManager()
	// Resolve options once and retain the configured manager across turns.
	// In particular, preserve restored history and custom compression managers.
	var contextManager loop.ContextManager
	loopOpts := append([]loop.Option(nil), cfg.LoopOptions...)
	loopOpts = append(loopOpts, func(c *loop.Config) {
		if c.ContextManager == nil {
			c.ContextManager = loop.NewDefaultContextManager()
		}
		contextManager = c.ContextManager
	})
	sessionLoop := loop.New(loopOpts...)

	se := &SessionEngine{
		id:             id,
		loop:           sessionLoop,
		contextManager: contextManager,
		budget:         bt,
		hooks:          hooks,
		agents:         NewAgentManager(bt, hooks),
		store:          store,
		state: &SessionState{
			ID:        id,
			Metadata:  make(map[string]any),
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
	}
	return se
}

// ID returns the unique identifier for this session engine.
func (se *SessionEngine) ID() string { return se.id }

// Hooks returns the session hook manager for registering lifecycle hooks.
func (se *SessionEngine) Hooks() *SessionHookManager { return se.hooks }

// Budget returns the budget tracker for this session.
func (se *SessionEngine) Budget() *BudgetTracker { return se.budget }

// Agents returns the agent manager for spawning and tracking subagents.
func (se *SessionEngine) Agents() *AgentManager { return se.agents }

// SubmitMessage starts a new turn with the given input and returns an event
// channel. The channel is closed when the turn finishes. Session hooks are
// fired around the turn execution, and state is persisted afterwards.
func (se *SessionEngine) SubmitMessage(ctx context.Context, input string) <-chan event.Event {
	out := make(chan event.Event, 64)
	go func() {
		defer close(out)

		// A ContextManager is mutable. Serialize turns so concurrent submissions
		// cannot interleave user/assistant messages in the shared history.
		se.turnMu.Lock()
		defer se.turnMu.Unlock()

		sessionCtx, cancel := context.WithCancel(ctx)
		se.mu.Lock()
		se.cancelFunc = cancel
		se.mu.Unlock()
		defer cancel()
		defer func() {
			se.mu.Lock()
			se.cancelFunc = nil
			se.mu.Unlock()
		}()

		turn := NewTurn(TurnConfig{
			Loop:   se.loop,
			Hooks:  se.hooks,
			Budget: se.budget,
		})

		_ = se.hooks.Fire(HookSessionStart, map[string]any{"session_id": se.id})

		for ev := range turn.Run(sessionCtx, input) {
			emitEvent(sessionCtx, out, ev)
		}

		se.mu.Lock()
		se.state.UpdatedAt = time.Now()
		se.state.Messages = se.contextManager.Messages()
		se.mu.Unlock()

		se.saveState()

		_ = se.hooks.Fire(HookSessionEnd, map[string]any{"session_id": se.id})
	}()

	return out
}

// Interrupt cancels the currently running turn, if any.
func (se *SessionEngine) Interrupt() {
	se.mu.RLock()
	cancel := se.cancelFunc
	se.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
}

// State returns a copy of the current session state.
func (se *SessionEngine) State() *SessionState {
	se.mu.RLock()
	defer se.mu.RUnlock()
	return cloneSessionState(se.state)
}

func (se *SessionEngine) saveState() {
	se.mu.RLock()
	state := cloneSessionState(se.state)
	se.mu.RUnlock()

	if se.store != nil {
		_ = se.store.Save(se.id, state)
	}
}
