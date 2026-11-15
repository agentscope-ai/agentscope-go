package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// ErrPoolClosed is returned by Submit when the pool has been closed.
var ErrPoolClosed = errors.New("agent pool is closed")

// AgentFactory creates a fresh agent instance for a pool worker.
type AgentFactory func() agent.Agent

// PoolResult holds the outcome of processing one submitted input.
type PoolResult struct {
	Input    string
	Output   *message.Msg
	Err      error
	Duration time.Duration
}

// PoolOption configures an AgentPool.
type PoolOption func(*AgentPool)

// Workers sets the number of worker goroutines. Defaults to 1.
func Workers(n int) PoolOption {
	return func(p *AgentPool) {
		if n > 0 {
			p.workers = n
		}
	}
}

// QueueSize sets the buffered work channel capacity. Defaults to the number of
// workers.
func QueueSize(n int) PoolOption {
	return func(p *AgentPool) {
		if n > 0 {
			p.queueSize = n
		}
	}
}

type job struct {
	ctx    context.Context
	input  string
	result chan PoolResult
}

// AgentPool is a work-queue that dispatches inputs to a fixed set of worker
// goroutines, each owning an agent created from the factory.
type AgentPool struct {
	factory   AgentFactory
	workers   int
	queueSize int

	jobs      chan job
	closedCh  chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// NewAgentPool constructs an AgentPool and starts its workers.
func NewAgentPool(factory AgentFactory, opts ...PoolOption) *AgentPool {
	p := &AgentPool{
		factory:  factory,
		workers:  1,
		closedCh: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.queueSize <= 0 {
		p.queueSize = p.workers
	}
	p.jobs = make(chan job, p.queueSize)
	p.start()
	return p
}

func (p *AgentPool) start() {
	p.startOnce.Do(func() {
		for i := 0; i < p.workers; i++ {
			p.wg.Add(1)
			go p.worker()
		}
	})
}

func (p *AgentPool) worker() {
	defer p.wg.Done()
	a := p.factory()
	for j := range p.jobs {
		start := time.Now()
		out, err := a.Reply(j.ctx, j.input)
		j.result <- PoolResult{
			Input:    j.input,
			Output:   out,
			Err:      err,
			Duration: time.Since(start),
		}
		close(j.result)
	}
}

// Submit enqueues an input for processing and returns a channel that will
// receive its single result. Returns ErrPoolClosed if the pool has been closed.
func (p *AgentPool) Submit(ctx context.Context, input string) (<-chan PoolResult, error) {
	result := make(chan PoolResult, 1)
	j := job{ctx: ctx, input: input, result: result}
	select {
	case p.jobs <- j:
		return result, nil
	case <-p.closedCh:
		return nil, ErrPoolClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close stops accepting new work and waits for in-flight jobs to finish.
func (p *AgentPool) Close() {
	p.closeOnce.Do(func() {
		close(p.closedCh)
		close(p.jobs)
	})
	p.wg.Wait()
}

// ---------------------------------------------------------------------------
// Handler-based Pool with backpressure, stats, and graceful shutdown.
// ---------------------------------------------------------------------------

// ErrPoolFull is returned by Pool.Submit when the queue is at capacity.
var ErrPoolFull = errors.New("agent pool queue is full")

// DefaultPoolConfig returns sensible defaults.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxWorkers:    10,
		QueueSize:     100,
		WorkerTimeout: 5 * time.Minute,
	}
}

// PoolConfig configures the handler-based Pool.
type PoolConfig struct {
	MaxWorkers    int           // max concurrent agent sessions (default: 10)
	QueueSize     int           // max pending requests before backpressure (default: 100)
	WorkerTimeout time.Duration // max time per request (default: 5 min)
}

// Request represents a work item for the Pool.
type Request struct {
	ID        string
	SessionID string
	Input     string
	Ctx       context.Context
	ResultCh  chan<- *Result // caller provides a channel to receive the result
}

// Result holds the outcome of processing a request.
type Result struct {
	RequestID string
	Output    string
	Error     error
	Duration  time.Duration
	TokensIn  int
	TokensOut int
}

// PoolStats holds runtime statistics for the Pool.
type PoolStats struct {
	ActiveWorkers int64
	PendingJobs   int64
	CompletedJobs int64
	FailedJobs    int64
	TotalDuration time.Duration
}

// Pool manages a set of worker goroutines that process agent requests using a
// caller-provided handler function. It provides backpressure via a bounded
// queue and exposes runtime statistics.
type Pool struct {
	cfg     PoolConfig
	queue   chan *Request
	wg      sync.WaitGroup
	done    chan struct{}
	closed  sync.Once
	handler func(ctx context.Context, req *Request) *Result

	activeWorkers atomic.Int64
	pendingJobs   atomic.Int64
	completedJobs atomic.Int64
	failedJobs    atomic.Int64
	totalDuration atomic.Int64 // nanoseconds
}

// NewPool creates and starts a Pool with the given handler function.
// The handler is invoked once per request inside a worker goroutine.
func NewPool(cfg PoolConfig, handler func(ctx context.Context, req *Request) *Result) *Pool {
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = 10
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 100
	}
	if cfg.WorkerTimeout <= 0 {
		cfg.WorkerTimeout = 5 * time.Minute
	}

	p := &Pool{
		cfg:     cfg,
		queue:   make(chan *Request, cfg.QueueSize),
		done:    make(chan struct{}),
		handler: handler,
	}

	for i := 0; i < cfg.MaxWorkers; i++ {
		p.wg.Add(1)
		go p.worker()
	}

	return p
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for req := range p.queue {
		p.pendingJobs.Add(-1)
		p.activeWorkers.Add(1)

		// Apply per-request timeout.
		ctx, cancel := context.WithTimeout(req.Ctx, p.cfg.WorkerTimeout)
		start := time.Now()
		result := p.handler(ctx, req)
		elapsed := time.Since(start)
		cancel()

		if result == nil {
			result = &Result{RequestID: req.ID}
		}
		result.Duration = elapsed

		if result.Error != nil {
			p.failedJobs.Add(1)
		} else {
			p.completedJobs.Add(1)
		}
		p.totalDuration.Add(int64(elapsed))
		p.activeWorkers.Add(-1)

		// Deliver result. Non-blocking in case caller abandoned the channel.
		if req.ResultCh != nil {
			select {
			case req.ResultCh <- result:
			default:
			}
		}
	}
}

// Submit adds a request to the Pool. Returns ErrPoolFull if the queue is at
// capacity (backpressure) or ErrPoolClosed if the pool has been shut down.
func (p *Pool) Submit(req *Request) error {
	select {
	case <-p.done:
		return ErrPoolClosed
	default:
	}

	select {
	case p.queue <- req:
		p.pendingJobs.Add(1)
		return nil
	case <-p.done:
		return ErrPoolClosed
	default:
		return ErrPoolFull
	}
}

// Stats returns a snapshot of current pool statistics.
func (p *Pool) Stats() PoolStats {
	return PoolStats{
		ActiveWorkers: p.activeWorkers.Load(),
		PendingJobs:   p.pendingJobs.Load(),
		CompletedJobs: p.completedJobs.Load(),
		FailedJobs:    p.failedJobs.Load(),
		TotalDuration: time.Duration(p.totalDuration.Load()),
	}
}

// Shutdown gracefully stops the pool. It stops accepting new work, then waits
// for all in-flight requests to complete. If ctx expires before workers finish,
// Shutdown returns ctx.Err() (workers will still finish in the background).
func (p *Pool) Shutdown(ctx context.Context) error {
	p.closed.Do(func() {
		close(p.done)
		close(p.queue)
	})

	waitCh := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(waitCh)
	}()

	select {
	case <-waitCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
