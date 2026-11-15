package runtime

import "sync"

// SessionHookEvent identifies the type of session lifecycle event.
type SessionHookEvent int

const (
	// HookSessionStart fires when a session begins.
	HookSessionStart SessionHookEvent = iota
	// HookSessionEnd fires when a session ends.
	HookSessionEnd
	// HookPreTurn fires before each conversation turn.
	HookPreTurn
	// HookPostTurn fires after each conversation turn completes.
	HookPostTurn
	// HookPreToolUse fires before a tool is executed.
	HookPreToolUse
	// HookPostToolUse fires after a tool execution completes.
	HookPostToolUse
	// HookBudgetWarning fires when resource consumption approaches the budget limit.
	HookBudgetWarning
	// HookSubagentStart fires when a sub-agent is spawned.
	HookSubagentStart
	// HookSubagentEnd fires when a sub-agent finishes.
	HookSubagentEnd
)

var sessionHookEventNames = [...]string{
	"session_start",
	"session_end",
	"pre_turn",
	"post_turn",
	"pre_tool_use",
	"post_tool_use",
	"budget_warning",
	"subagent_start",
	"subagent_end",
}

// String returns the snake_case name of the event.
func (e SessionHookEvent) String() string {
	if int(e) < len(sessionHookEventNames) {
		return sessionHookEventNames[e]
	}
	return "unknown"
}

// SessionHook is the interface for hooks that react to session lifecycle events.
type SessionHook interface {
	On(event SessionHookEvent, payload any) error
	Events() []SessionHookEvent
}

// FuncHook is a convenience SessionHook backed by a plain function.
type FuncHook struct {
	Fn   func(SessionHookEvent, any) error
	Evts []SessionHookEvent
}

// On calls the wrapped function.
func (h *FuncHook) On(event SessionHookEvent, payload any) error {
	return h.Fn(event, payload)
}

// Events returns the set of events this hook subscribes to.
func (h *FuncHook) Events() []SessionHookEvent {
	return h.Evts
}

// SessionHookManager manages a set of SessionHooks and dispatches events to
// the subset of hooks that subscribed to each event type.
type SessionHookManager struct {
	mu    sync.RWMutex
	hooks []SessionHook
	index map[SessionHookEvent][]SessionHook
}

// NewSessionHookManager creates an empty SessionHookManager.
func NewSessionHookManager() *SessionHookManager {
	return &SessionHookManager{
		index: make(map[SessionHookEvent][]SessionHook),
	}
}

// Register adds a hook. It is indexed by the events returned from hook.Events().
func (m *SessionHookManager) Register(hook SessionHook) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hooks = append(m.hooks, hook)
	for _, e := range hook.Events() {
		m.index[e] = append(m.index[e], hook)
	}
}

// Fire dispatches an event to every hook subscribed to it, in registration
// order. If any hook returns an error the chain stops and the error is returned.
func (m *SessionHookManager) Fire(event SessionHookEvent, payload any) error {
	m.mu.RLock()
	hooks := m.index[event]
	m.mu.RUnlock()

	for _, h := range hooks {
		if err := h.On(event, payload); err != nil {
			return err
		}
	}
	return nil
}

// HookCount returns the total number of registered hooks.
func (m *SessionHookManager) HookCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.hooks)
}
