// Package hotreload provides a polling-based config file watcher that detects
// changes to configuration files and notifies subscribers, enabling live
// updates without restarting the process.
package hotreload

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// ChangeEvent is emitted when a watched file changes.
type ChangeEvent struct {
	Path      string
	Timestamp time.Time
}

// Handler is called when a config file changes.
type Handler func(event ChangeEvent, data []byte) error

// WatcherConfig configures the hot-reload watcher.
type WatcherConfig struct {
	PollInterval time.Duration // how often to check (default: 2s)
}

// Watcher polls files for changes and notifies handlers.
type Watcher struct {
	cfg      WatcherConfig
	files    map[string]*fileState
	handlers map[string][]Handler
	mu       sync.RWMutex
	cancel   context.CancelFunc
	done     chan struct{}
}

type fileState struct {
	path    string
	modTime time.Time
	size    int64
	exists  bool
}

// NewWatcher creates a new config file watcher.
func NewWatcher(cfg WatcherConfig) *Watcher {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	return &Watcher{
		cfg:      cfg,
		files:    make(map[string]*fileState),
		handlers: make(map[string][]Handler),
	}
}

// Watch registers a file path and a handler to call when it changes.
// Multiple handlers can be registered for the same path.
func (w *Watcher) Watch(path string, handler Handler) error {
	if path == "" {
		return fmt.Errorf("hotreload: path must not be empty")
	}
	if handler == nil {
		return fmt.Errorf("hotreload: handler must not be nil")
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// Snapshot the current file state (file may not exist yet).
	if _, tracked := w.files[path]; !tracked {
		w.files[path] = snapshotFile(path)
	}

	w.handlers[path] = append(w.handlers[path], handler)
	return nil
}

// Unwatch removes all handlers for a path and stops tracking it.
func (w *Watcher) Unwatch(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.handlers, path)
	delete(w.files, path)
}

// Start begins the polling loop. It blocks until the context is canceled
// or Stop is called. Call Start in a goroutine.
func (w *Watcher) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)

	w.mu.Lock()
	w.cancel = cancel
	w.done = make(chan struct{})
	w.mu.Unlock()

	defer close(w.done)

	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_ = w.ForceCheck()
		}
	}
}

// Stop terminates the watcher and waits for the polling loop to exit.
func (w *Watcher) Stop() {
	w.mu.RLock()
	cancel := w.cancel
	done := w.done
	w.mu.RUnlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// ForceCheck immediately checks all watched files and fires handlers for any
// that have changed. Returns a slice of errors from handlers that failed.
func (w *Watcher) ForceCheck() []error {
	w.mu.RLock()
	paths := make([]string, 0, len(w.files))
	for p := range w.files {
		paths = append(paths, p)
	}
	w.mu.RUnlock()

	var errs []error

	for _, path := range paths {
		newState := snapshotFile(path)

		w.mu.RLock()
		oldState, ok := w.files[path]
		w.mu.RUnlock()
		if !ok {
			continue
		}

		changed := false
		switch {
		case !oldState.exists && newState.exists:
			// File was created.
			changed = true
		case oldState.exists && newState.exists &&
			(oldState.modTime != newState.modTime || oldState.size != newState.size):
			// File was modified.
			changed = true
		}

		if !changed {
			continue
		}

		// Read file content before invoking handlers.
		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("hotreload: read %s: %w", path, err))
			continue
		}

		event := ChangeEvent{
			Path:      path,
			Timestamp: time.Now(),
		}

		w.mu.RLock()
		handlers := make([]Handler, len(w.handlers[path]))
		copy(handlers, w.handlers[path])
		w.mu.RUnlock()

		for _, h := range handlers {
			if herr := h(event, data); herr != nil {
				errs = append(errs, fmt.Errorf("hotreload: handler for %s: %w", path, herr))
			}
		}

		// Update stored state after successful notification.
		w.mu.Lock()
		w.files[path] = newState
		w.mu.Unlock()
	}

	return errs
}

// snapshotFile captures the current mtime and size of a file.
func snapshotFile(path string) *fileState {
	info, err := os.Stat(path)
	if err != nil {
		return &fileState{path: path, exists: false}
	}
	return &fileState{
		path:    path,
		modTime: info.ModTime(),
		size:    info.Size(),
		exists:  true,
	}
}
