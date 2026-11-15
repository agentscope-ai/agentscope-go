package managedtask

import (
	"context"
	"sync"
)

type waiter struct {
	ready   chan struct{}
	granted bool
}
type admission struct {
	mu                           sync.Mutex
	active, maxActive, maxQueued int
	queue                        []*waiter
}

func (a *admission) acquire(ctx context.Context) error {
	a.mu.Lock()
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return err
	}
	if a.active < a.maxActive && len(a.queue) == 0 {
		a.active++
		a.mu.Unlock()
		return nil
	}
	if len(a.queue) >= a.maxQueued {
		a.mu.Unlock()
		return ErrCapacity
	}
	w := &waiter{ready: make(chan struct{})}
	a.queue = append(a.queue, w)
	a.mu.Unlock()
	select {
	case <-w.ready:
		if err := ctx.Err(); err != nil {
			a.release()
			return err
		}
		return nil
	case <-ctx.Done():
		a.mu.Lock()
		if w.granted {
			a.mu.Unlock()
			a.release()
		} else {
			for i, queued := range a.queue {
				if queued == w {
					a.queue = append(a.queue[:i], a.queue[i+1:]...)
					break
				}
			}
			a.mu.Unlock()
		}
		return ctx.Err()
	}
}

func (a *admission) release() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.queue) > 0 {
		w := a.queue[0]
		a.queue = a.queue[1:]
		w.granted = true
		close(w.ready)
	} else {
		a.active--
	}
}
