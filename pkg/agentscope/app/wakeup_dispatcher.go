package app

import (
	"sync"
)

// WakeupDispatcher routes wakeup signals to waiting sessions.
// When a session's agent is paused (e.g., waiting for external execution),
// the dispatcher delivers a wakeup signal so the agent can resume.
type WakeupDispatcher struct {
	mu       sync.Mutex
	channels map[string]chan struct{} // sessionID -> wakeup channel
}

// NewWakeupDispatcher creates a new wakeup dispatcher.
func NewWakeupDispatcher() *WakeupDispatcher {
	return &WakeupDispatcher{
		channels: make(map[string]chan struct{}),
	}
}

// Register creates a wakeup channel for a session.
// Returns the channel that can be selected on.
func (d *WakeupDispatcher) Register(sessionID string) <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	ch := make(chan struct{}, 1)
	d.channels[sessionID] = ch
	return ch
}

// Wakeup sends a wakeup signal to a session.
//
// The send happens while holding d.mu: Unregister closes the channel under
// the same mutex, so an unguarded send after releasing the lock could race
// with close and panic with "send on closed channel" (upstream #2476 class).
// The send is non-blocking (buffered 1 + default), so holding the lock here
// cannot stall other sessions.
func (d *WakeupDispatcher) Wakeup(sessionID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ch, ok := d.channels[sessionID]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Unregister removes the wakeup channel for a session.
func (d *WakeupDispatcher) Unregister(sessionID string) {
	d.mu.Lock()
	if ch, ok := d.channels[sessionID]; ok {
		close(ch)
		delete(d.channels, sessionID)
	}
	d.mu.Unlock()
}
