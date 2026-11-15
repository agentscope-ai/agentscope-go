package hotreload

import (
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
)

// Reloader is a typed config reloader that unmarshals config changes into T.
type Reloader[T any] struct {
	watcher  *Watcher
	path     string
	current  atomic.Pointer[T]
	onChange func(old, new_ *T)
	parse    func(data []byte) (*T, error)
}

// ReloaderOption configures a Reloader.
type ReloaderOption[T any] func(*Reloader[T])

// WithOnChange sets a callback invoked when the config changes.
// The callback receives the previous and new values.
func WithOnChange[T any](fn func(old, new_ *T)) ReloaderOption[T] {
	return func(r *Reloader[T]) {
		r.onChange = fn
	}
}

// WithParser sets a custom parser function. The default is JSON.
func WithParser[T any](fn func([]byte) (*T, error)) ReloaderOption[T] {
	return func(r *Reloader[T]) {
		r.parse = fn
	}
}

// NewReloader creates a typed config reloader for a specific file.
// It performs an initial load of the file and registers a handler with the
// watcher. The watcher must be started separately.
func NewReloader[T any](watcher *Watcher, path string, opts ...ReloaderOption[T]) (*Reloader[T], error) {
	r := &Reloader[T]{
		watcher: watcher,
		path:    path,
		parse:   defaultParse[T],
	}

	for _, opt := range opts {
		opt(r)
	}

	// Initial load.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("hotreload: initial load of %s: %w", path, err)
	}

	val, err := r.parse(data)
	if err != nil {
		return nil, fmt.Errorf("hotreload: parse %s: %w", path, err)
	}
	r.current.Store(val)

	// Register the reload handler with the watcher.
	if err := watcher.Watch(path, r.handleChange); err != nil {
		return nil, err
	}

	return r, nil
}

// Get returns the current config value. It is safe for concurrent use.
func (r *Reloader[T]) Get() *T {
	return r.current.Load()
}

func (r *Reloader[T]) handleChange(_ ChangeEvent, data []byte) error {
	newVal, err := r.parse(data)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}

	old := r.current.Swap(newVal)

	if r.onChange != nil {
		r.onChange(old, newVal)
	}
	return nil
}

func defaultParse[T any](data []byte) (*T, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return &v, nil
}
