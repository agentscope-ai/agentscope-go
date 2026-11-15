package app

import (
	"context"
	"sync"
)

// ChatRunRegistry tracks active chat runs to prevent duplicate concurrent
// chats for the same session. Each session can have at most one active chat.
type ChatRunRegistry struct {
	mu      sync.Mutex
	running map[string]*chatRun
}

// Each admitted execution owns a distinct lease, including its cancel handle.
type chatRun struct{ cancel context.CancelFunc }

// NewChatRunRegistry creates a new chat run registry.
func NewChatRunRegistry() *ChatRunRegistry {
	return &ChatRunRegistry{
		running: make(map[string]*chatRun),
	}
}

// TryAcquire attempts to start a chat run for a session.
// Returns true if acquired, false if a chat is already running.
func (r *ChatRunRegistry) TryAcquire(sessionID string) bool {
	return r.acquire(sessionID, nil) != nil
}

func (r *ChatRunRegistry) acquire(sessionID string, cancel context.CancelFunc) *chatRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running[sessionID] != nil {
		return nil
	}
	run := &chatRun{cancel: cancel}
	r.running[sessionID] = run
	return run
}

func (r *ChatRunRegistry) release(sessionID string, run *chatRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running[sessionID] == run {
		delete(r.running, sessionID)
	}
}

func (r *ChatRunRegistry) cancel(sessionID string) {
	r.mu.Lock()
	run := r.running[sessionID]
	r.mu.Unlock()
	// Invoke outside the lock. A concurrent release/new admission cannot
	// redirect this cancellation to a different execution.
	if run != nil && run.cancel != nil {
		run.cancel()
	}
}

// Release ends the chat run for a session.
func (r *ChatRunRegistry) Release(sessionID string) {
	r.mu.Lock()
	delete(r.running, sessionID)
	r.mu.Unlock()
}

// IsRunning returns whether a chat is active for a session.
func (r *ChatRunRegistry) IsRunning(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[sessionID] != nil
}
