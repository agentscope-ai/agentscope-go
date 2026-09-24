package inference

import (
	"context"
	"sync"
)

// Stats is an instantaneous pool snapshot. Active includes locally retained
// streams; queued work excludes active work and canceled/removed waiters.
type Stats struct {
	Active     int
	Queued     int
	QueuedWork int64
}
type waiter struct {
	ready   chan struct{}
	work    int64
	granted bool
	err     error
}
type pool struct {
	mu         sync.Mutex
	cfg        PoolConfig
	active     int
	queue      []*waiter
	queuedWork int64
	closed     bool
	ctx        context.Context
	cancel     context.CancelFunc
}

func (p *pool) stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{Active: p.active, Queued: len(p.queue), QueuedWork: p.queuedWork}
}
func (p *pool) acquire(ctx context.Context, work int64) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if work < 1 {
		work = 1
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	if p.active < p.cfg.MaxActive && len(p.queue) == 0 {
		p.active++
		p.mu.Unlock()
		return p.releaser(), nil
	}
	if len(p.queue) >= p.cfg.MaxQueued || work > p.cfg.MaxQueuedWork-p.queuedWork {
		p.mu.Unlock()
		return nil, ErrQueueFull
	}
	w := &waiter{ready: make(chan struct{}), work: work}
	p.queue = append(p.queue, w)
	p.queuedWork += work
	p.mu.Unlock()
	select {
	case <-w.ready:
	case <-ctx.Done():
	}
	p.mu.Lock()
	if err := ctx.Err(); err != nil {
		if w.granted {
			p.active--
			p.dispatch()
		} else {
			for i, q := range p.queue {
				if q == w {
					p.queue = append(p.queue[:i], p.queue[i+1:]...)
					p.queuedWork -= work
					break
				}
			}
		}
		p.mu.Unlock()
		return nil, err
	}
	err := w.err
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return p.releaser(), nil
}
func (p *pool) releaser() func() {
	var once sync.Once
	return func() { once.Do(func() { p.mu.Lock(); defer p.mu.Unlock(); p.active--; p.dispatch() }) }
}
func (p *pool) dispatch() {
	for !p.closed && p.active < p.cfg.MaxActive && len(p.queue) > 0 {
		w := p.queue[0]
		p.queue[0] = nil
		p.queue = p.queue[1:]
		p.queuedWork -= w.work
		p.active++
		w.granted = true
		close(w.ready)
	}
}
func (p *pool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	p.cancel()
	for _, w := range p.queue {
		w.err = ErrClosed
		close(w.ready)
	}
	p.queue = nil
	p.queuedWork = 0
}
