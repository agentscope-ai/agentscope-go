package inference

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type operationKey struct{}

// Operation is a single-target logical call and total send budget. Adapter
// helpers may share it sequentially; parallel batches use separate operations.
type Operation struct {
	cancel     context.CancelFunc
	stop       func() bool
	mu         sync.Mutex
	deployment *Deployment
	id         uint64
	purpose    string
	attempts   int
	finished   bool
	stream     *streamAttempt
}
type streamAttempt struct {
	id       uint64
	release  func()
	cancel   context.CancelFunc
	stop     func() bool
	done     chan struct{}
	usage    usageSample
	terminal bool
	invalid  bool
	status   int
}

// Start creates an operation, or reuses an existing operation for this target.
// The returned finish must be called after all local producers have completed.
// Sharing this context across concurrent independent calls is unsupported.
func (d *Deployment) Start(ctx context.Context, purpose string) (context.Context, func(error)) {
	if prior := CurrentOperation(ctx); prior != nil && prior.deployment == d {
		return ctx, func(error) {}
	}
	if p := PurposeFromContext(ctx); p != "" {
		purpose = p
	}
	if purpose == "" {
		purpose = "reasoning"
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(d.pool.ctx, cancel)
	record := &OperationRecord{TargetID: d.cfg.Descriptor.ID, Model: d.cfg.Descriptor.Model, Identity: IdentityFromContext(ctx), Attribution: AttributionFromContext(ctx), Purpose: purpose, StartedAt: time.Now(), Outcome: "pending"}
	op := &Operation{cancel: cancel, stop: stop, deployment: d, id: d.ledger.beginOperation(record), purpose: purpose}
	return context.WithValue(ctx, operationKey{}, op), op.finish
}
func CurrentOperation(ctx context.Context) *Operation {
	op, _ := ctx.Value(operationKey{}).(*Operation)
	return op
}
func (op *Operation) finish(err error) {
	defer op.cancel()
	defer op.stop()
	op.mu.Lock()
	if op.finished {
		op.mu.Unlock()
		return
	}
	op.finished = true
	s := op.stream
	op.mu.Unlock()
	if s != nil {
		s.cancel()
		<-s.done
		s.stop()
		s.release()
		op.mu.Lock()
		op.stream = nil
		op.mu.Unlock()
		outcome := "success"
		if err != nil {
			outcome = errorOutcome(err)
		}
		op.deployment.ledger.finish(s.id, &s.usage.usage, s.usage.known(), s.status, outcome, op.deployment.cfg.Price)
	}
	outcome := "success"
	if err != nil {
		outcome = errorOutcome(err)
	}
	op.deployment.ledger.finishOperation(op.id, outcome)
}
func errorOutcome(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	return "error"
}
func (op *Operation) next(ctx context.Context) (uint64, func(), context.Context, context.CancelFunc, func() bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, nil, nil, nil, err
	}
	if IdentityFromContext(ctx).TenantID == "" {
		return 0, nil, nil, nil, nil, ErrIdentityRequired
	}
	op.mu.Lock()
	if op.finished || op.stream != nil {
		op.mu.Unlock()
		return 0, nil, nil, nil, nil, fmt.Errorf("inference: operation is finished or already streaming")
	}
	if op.attempts >= op.deployment.cfg.MaxAttempts {
		op.mu.Unlock()
		return 0, nil, nil, nil, nil, ErrAttemptLimit
	}
	op.mu.Unlock()
	release, err := op.deployment.acquire(ctx, estimatedWork(ctx))
	if err != nil {
		return 0, nil, nil, nil, nil, err
	}
	requestCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(op.deployment.pool.ctx, cancel)
	if err := requestCtx.Err(); err != nil {
		stop()
		cancel()
		release()
		return 0, nil, nil, nil, nil, err
	}
	op.mu.Lock()
	if op.finished || op.attempts >= op.deployment.cfg.MaxAttempts {
		op.mu.Unlock()
		stop()
		cancel()
		release()
		return 0, nil, nil, nil, nil, ErrAttemptLimit
	}
	op.attempts++
	number := op.attempts
	op.mu.Unlock()
	desc := op.deployment.cfg.Descriptor
	purpose := PurposeFromContext(ctx)
	if purpose == "" {
		purpose = op.purpose
	}
	id := op.deployment.ledger.begin(&Attempt{OperationID: op.id, Number: number, TargetID: desc.ID, PoolID: op.deployment.cfg.PoolID, Provider: desc.Provider, API: desc.API, Model: desc.Model, Identity: IdentityFromContext(ctx), Attribution: AttributionFromContext(ctx), Purpose: purpose, StartedAt: time.Now(), Outcome: "pending"})
	return id, release, requestCtx, cancel, stop, nil
}

// StreamTransportDone must be called after the managed SSE parser has closed its
// response body. The model wrapper waits for this before releasing admission.
func (op *Operation) StreamTransportDone() {
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.stream != nil {
		select {
		case <-op.stream.done:
		default:
			close(op.stream.done)
		}
	}
}

// StreamError validates raw protocol termination independently of adapter IsLast.
func (op *Operation) StreamError() error {
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.stream == nil || !op.stream.terminal || op.stream.invalid {
		return fmt.Errorf("inference: stream ended without a valid provider terminal")
	}
	return nil
}

// ObserveStreamData records raw protocol evidence before adapter forwarding.
func (op *Operation) ObserveStreamData(data string) {
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.stream != nil {
		op.observeStream(data)
	}
}

// StartIndependent creates a fresh operation even when ctx carries an enclosing
// operation. Embedding batches use this to keep parallel send caps independent.
func (d *Deployment) StartIndependent(ctx context.Context, purpose string) (context.Context, func(error)) {
	return d.Start(context.WithValue(ctx, operationKey{}, (*Operation)(nil)), purpose)
}
