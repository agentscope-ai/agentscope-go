// Package runctx joins agent cores owned by an application chat request.
package runctx

import (
	"context"
	"sync"
)

type contextKey struct{}

// Scope tracks accepted agent cores, including children sharing its context.
// Sealing prevents delayed middleware from starting work after request cleanup.
type Scope struct {
	mu     sync.Mutex
	active int
	sealed bool
	done   chan struct{}
}

// New attaches a fresh execution scope to ctx. It does not cancel ctx.
func New(ctx context.Context) (context.Context, *Scope) {
	s := &Scope{done: make(chan struct{})}
	return context.WithValue(ctx, contextKey{}, s), s
}

// Start registers a core before it is launched. Its release is idempotent.
// Without an application scope this is a no-op; a sealed scope rejects work.
func Start(ctx context.Context) (release func(), accepted bool) {
	s, _ := ctx.Value(contextKey{}).(*Scope)
	if s == nil {
		return func() {}, true
	}
	s.mu.Lock()
	if s.sealed {
		s.mu.Unlock()
		return nil, false
	}
	s.active++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.active--
			if s.sealed && s.active == 0 {
				close(s.done)
			}
		})
	}, true
}

// Close seals the scope and waits for every accepted core to exit. The owner
// cancels its run context first. A tool ignoring cancellation keeps ownership
// until it returns; there is deliberately no timeout that would permit overlap.
func (s *Scope) Close() {
	s.mu.Lock()
	if !s.sealed {
		s.sealed = true
		if s.active == 0 {
			close(s.done)
		}
	}
	s.mu.Unlock()
	<-s.done
}
