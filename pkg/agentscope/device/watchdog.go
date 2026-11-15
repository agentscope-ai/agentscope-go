package device

import (
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Watchdog is a timer-based safety mechanism for physical devices.
// If Kick() is not called within the configured timeout, the watchdog triggers
// the safeState function to put actuators into a known-safe configuration
// (e.g. motors off, valves closed).
//
// This prevents runaway actuator commands if the agent loop hangs, loses
// connectivity, or crashes.
type Watchdog struct {
	timeout   time.Duration
	safeState func()

	mu      sync.Mutex
	timer   *time.Timer
	running bool
}

// NewWatchdog creates a watchdog with the given timeout and safe-state callback.
// The watchdog does not start until Start() is called.
func NewWatchdog(timeout time.Duration, safeState func()) *Watchdog {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Watchdog{
		timeout:   timeout,
		safeState: safeState,
	}
}

// Start begins the watchdog timer. If Kick() is not called within timeout,
// the safe-state function fires.
func (w *Watchdog) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.running {
		return
	}

	w.timer = time.AfterFunc(w.timeout, w.trigger)
	w.running = true
}

// Stop halts the watchdog timer without triggering safe-state.
func (w *Watchdog) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.running {
		return
	}
	w.timer.Stop()
	w.running = false
}

// Kick resets the watchdog timer, indicating the system is still healthy.
// Must be called periodically by the agent loop.
func (w *Watchdog) Kick() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.running {
		return
	}
	w.timer.Reset(w.timeout)
}

// Running returns whether the watchdog is currently active.
func (w *Watchdog) Running() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

// trigger is called when the timer fires (timeout without Kick).
func (w *Watchdog) trigger() {
	logrus.Warn("watchdog: timeout expired, triggering safe state")
	w.mu.Lock()
	w.running = false
	w.mu.Unlock()

	if w.safeState != nil {
		w.safeState()
	}
}
