package managedtask

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// Executor owns one immutable deployment and bounded admission. Construct with
// New; methods are safe for concurrent calls. Share the instance across callers.
type Executor struct {
	cfg       Config
	limits    Limits
	tasks     chan struct{}
	admission admission
}

// New validates and snapshots configuration without calling host code or sending.
func New(cfg Config) (*Executor, error) { //nolint:gocritic // explicit public configuration value
	if cfg.Client == nil || cfg.Authorize == nil || cfg.Project == nil || cfg.InputTokenBound == nil || cfg.StrictBudget != nil {
		return nil, ErrConfig
	}
	endpoint, name := cfg.Client.Target()
	d := &cfg.Deployment
	if strings.TrimSpace(d.ID) == "" || d.Endpoint != endpoint || d.Model != name || d.ContextWindow <= 0 || !d.TextOnly || !d.NonStreaming || !d.ToolsDisabled {
		return nil, ErrConfig
	}
	limits := DefaultLimits()
	if cfg.Limits != nil {
		limits = *cfg.Limits
	}
	if limits.MaxTasks <= 0 || limits.MaxQueued < 0 || limits.MaxConcurrent <= 0 || limits.MaxAttempts <= 0 || limits.MaxRequestBytes <= 0 || limits.MaxResponseBytes <= 0 || limits.MaxResponseBytes == math.MaxInt || limits.MaxOutputTokens <= 0 || limits.TaskTimeout <= 0 || limits.QueueTimeout <= 0 || limits.RetryBackoff <= 0 {
		return nil, ErrConfig
	}
	for i := range d.Qualifications {
		q := &d.Qualifications[i]
		if q.DeploymentID != d.ID || q.Endpoint != endpoint || q.Model != name || strings.TrimSpace(q.Category) == "" || strings.TrimSpace(q.EvidenceID) == "" || strings.TrimSpace(q.DatasetVersion) == "" || strings.TrimSpace(q.RubricVersion) == "" || q.SampleSize <= 0 || !finite(q.MaxQualityLoss) || q.MaxQualityLoss < 0 || q.MaxQualityLoss > 1 || q.MaxLatency <= 0 {
			return nil, ErrConfig
		}
	}
	d.Qualifications = append([]Qualification(nil), d.Qualifications...)
	cfg.Limits = nil
	if cfg.Prices != nil {
		p := *cfg.Prices
		if strings.TrimSpace(p.Currency) == "" {
			return nil, ErrConfig
		}
		rates := []**float64{&p.InputPerToken, &p.CachedInputPerToken, &p.OutputPerToken}
		for _, rate := range rates {
			if *rate == nil {
				continue
			}
			n := **rate
			if !finite(n) || n < 0 {
				return nil, ErrConfig
			}
			*rate = &n
		}
		cfg.Prices = &p
	}
	return &Executor{cfg: cfg, limits: limits, tasks: make(chan struct{}, limits.MaxTasks), admission: admission{maxActive: limits.MaxConcurrent, maxQueued: limits.MaxQueued}}, nil
}

func finite(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) }
func (e *Executor) deployment() Deployment {
	d := e.cfg.Deployment
	d.Qualifications = append([]Qualification(nil), d.Qualifications...)
	return d
}

// Execute observes one terminal outcome within the caller/TaskTimeout deadline.
// A nil parent is rejected with ErrTask before callbacks, admission or dispatch.
// A bounded worker retains task/permit ownership until local callbacks and Send
// finish. Uncooperative host hooks can delay cleanup, but cannot release capacity
// early, trigger later retries or mutate returned snapshots. No whole Agent runs.
func (e *Executor) Execute(parent context.Context, task Task) *Result {
	if e == nil {
		return &Result{TaskID: task.ID, Status: Rejected, Err: ErrConfig}
	}
	if parent == nil {
		return &Result{TaskID: task.ID, Status: Rejected, Err: ErrTask}
	}
	ctx, cancel := context.WithTimeout(parent, e.limits.TaskTimeout)
	defer cancel()
	state := &execution{result: Result{TaskID: task.ID}, ctx: ctx, executor: e}
	if err := ctx.Err(); err != nil {
		return state.finish(Canceled, "", err)
	}
	if task.Mode == "" {
		task.Mode = Production
	}
	if strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.Category) == "" || strings.TrimSpace(task.Text) == "" || !utf8.ValidString(task.Text) || (task.Mode != Production && task.Mode != Evaluation) || len(task.Text) > e.limits.MaxRequestBytes {
		return state.finish(Rejected, "", ErrTask)
	}
	select {
	case e.tasks <- struct{}{}:
	default:
		return state.finish(Rejected, "", ErrCapacity)
	}
	done := make(chan struct{})
	go func() { defer func() { <-e.tasks; close(done) }(); e.run(ctx, task, state) }()
	select {
	case <-done:
		return state.snapshot()
	case <-ctx.Done():
		return state.finish(Canceled, "", ctx.Err())
	}
}

func (e *Executor) qualified(task Task) bool {
	if task.Mode == Evaluation {
		return true
	}
	for i := range e.cfg.Deployment.Qualifications {
		q := &e.cfg.Deployment.Qualifications[i]
		if q.Category == task.Category {
			return true
		}
	}
	return false
}

func (e *Executor) authorize(ctx context.Context, task Task) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !e.qualified(task) {
		return ErrUnqualified
	}
	if err := e.cfg.Authorize(ctx, task, e.deployment()); err != nil {
		return hostError(ctx, err, ErrDenied)
	}
	return ctx.Err()
}

func hostError(ctx context.Context, err, fallback error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fallback
}

func (e *Executor) run(ctx context.Context, task Task, state *execution) {
	if err := e.authorize(ctx, task); err != nil {
		state.finish(Rejected, "", err)
		return
	}
	projected, err := e.cfg.Project(ctx, task, e.deployment())
	if err != nil {
		state.finish(Rejected, "", hostError(ctx, err, ErrProjection))
		return
	}
	if err = ctx.Err(); err != nil {
		state.finish(Canceled, "", err)
		return
	}
	prepared, err := e.cfg.Client.Prepare(ctx, projected, e.limits.MaxOutputTokens, e.limits.MaxRequestBytes)
	if err != nil {
		state.finish(Rejected, "", err)
		return
	}
	bound, err := e.cfg.InputTokenBound(ctx, e.deployment(), prepared.JSON())
	if err != nil {
		state.finish(Rejected, "", hostError(ctx, err, ErrContext))
		return
	}
	if bound < 0 || e.limits.MaxOutputTokens > e.cfg.Deployment.ContextWindow || bound > e.cfg.Deployment.ContextWindow-e.limits.MaxOutputTokens {
		state.finish(Rejected, "", ErrContext)
		return
	}
	for attempt := 0; attempt < e.limits.MaxAttempts; attempt++ {
		if err = ctx.Err(); err != nil {
			state.finish(Canceled, "", err)
			return
		}
		queueCtx, stopQueue := context.WithTimeout(ctx, e.limits.QueueTimeout)
		err = e.admission.acquire(queueCtx)
		stopQueue()
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			} else if errors.Is(err, context.DeadlineExceeded) {
				err = ErrQueueTimeout
			}
			state.finish(Rejected, "", err)
			return
		}
		response, callErr := e.send(ctx, task, prepared, state)
		e.admission.release()
		if callErr == nil {
			state.finish(Succeeded, response.Text, nil)
			return
		}
		if ctx.Err() != nil || errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded) {
			state.finish(Canceled, "", hostError(ctx, callErr, callErr))
			return
		}
		if !ae.IsRetryable(callErr) || attempt+1 == e.limits.MaxAttempts {
			status := Failed
			switch {
			case errors.Is(callErr, ErrDenied), errors.Is(callErr, ErrUnqualified):
				status = Rejected
			case errors.Is(callErr, model.ErrTextUnsupported):
				status = Unsupported
			case errors.Is(callErr, model.ErrTextIncomplete):
				if response.FinishReason == model.StopReasonLength {
					status = Truncated
				} else {
					status = Filtered
				}
			}
			state.finish(status, "", callErr)
			return
		}
		delay := e.limits.RetryBackoff
		if retryAfter, ok := ae.RetryAfterOf(callErr); ok && retryAfter > delay {
			delay = retryAfter
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			state.finish(Canceled, "", ctx.Err())
			return
		}
	}
}

func (e *Executor) send(ctx context.Context, task Task, p *model.PreparedTextRequest, state *execution) (*model.TextCallResult, error) {
	if err := e.authorize(ctx, task); err != nil {
		return &model.TextCallResult{}, err
	}
	index := -1
	result, err := e.cfg.Client.Send(ctx, p, e.limits.MaxResponseBytes, func() error {
		state.mu.Lock()
		defer state.mu.Unlock()
		if cause := ctx.Err(); cause != nil {
			return cause
		}
		if state.terminal || len(state.result.Attempts) >= e.limits.MaxAttempts {
			return ErrCapacity
		}
		index = len(state.result.Attempts)
		state.starts = append(state.starts, time.Now())
		state.result.Attempts = append(state.result.Attempts, Attempt{Kind: PhysicalAttempt, Number: index + 1, DeploymentID: e.cfg.Deployment.ID, Endpoint: e.cfg.Deployment.Endpoint, RequestedModel: e.cfg.Deployment.Model})
		return nil
	})
	state.mu.Lock()
	if index >= 0 && !state.terminal {
		a := &state.result.Attempts[index]
		a.Model = result.Model
		a.Usage = result.Usage
		a.Err = err
		a.Completed = true
		a.Duration = time.Since(state.starts[index])
		if err == nil && result.Model != e.cfg.Deployment.Model {
			result.Text = ""
			err = model.ErrTextInvalidResponse
			a.Err = err
		}
	}
	state.mu.Unlock()
	return result, err
}

// execution serializes dispatch commitment and terminal observation. No worker
// may write the accepted result after terminal=true; unfinished calls stay unknown.
type execution struct {
	mu       sync.Mutex
	result   Result
	starts   []time.Time
	terminal bool
	ctx      context.Context
	executor *Executor
}

func (s *execution) finish(status Status, text string, err error) *Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.terminal {
		if cause := s.ctx.Err(); cause != nil {
			err = cause
		}
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			status = TimedOut
		case errors.Is(err, context.Canceled):
			status = Canceled
		}
		if status != Succeeded {
			text = ""
		}
		s.result.Status = status
		s.result.Text = text
		s.result.Err = err
		for i := range s.result.Attempts {
			if !s.result.Attempts[i].Completed {
				s.result.Attempts[i].Duration = time.Since(s.starts[i])
				s.result.Attempts[i].Err = err
			}
		}
		s.terminal = true
	}
	return s.copyLocked()
}

func (s *execution) snapshot() *Result { s.mu.Lock(); defer s.mu.Unlock(); return s.copyLocked() }
func (s *execution) copyLocked() *Result {
	r := s.result
	r.Attempts = append([]Attempt(nil), r.Attempts...)
	for i := range r.Attempts {
		if u := r.Attempts[i].Usage; u != nil {
			copyUsage := *u
			copyUsage.InputTokens = copyCount(u.InputTokens)
			copyUsage.OutputTokens = copyCount(u.OutputTokens)
			copyUsage.CachedInputTokens = copyCount(u.CachedInputTokens)
			r.Attempts[i].Usage = &copyUsage
		}
	}
	r.Cost = s.executor.cost(r.Attempts)
	return &r
}
func copyCount(n *int64) *int64 {
	if n == nil {
		return nil
	}
	v := *n
	return &v
}
