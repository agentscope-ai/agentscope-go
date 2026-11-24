package agent

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent/internal/retentioncopy"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// RetentionCopyLimits (EXPERIMENTAL) bounds each copy. Allocation bytes are
// logical charges, not an RSS bound. All fields must be positive; depth <=128.
type RetentionCopyLimits = retentioncopy.Limits

// RetentionPreviewConfig (EXPERIMENTAL) bounds owned ingress and snapshot copies.
// TTL must be positive and at most one minute. Only one handle is retained;
// expiry is reclaimed lazily, without a timer or background goroutine.
type RetentionPreviewConfig struct {
	Limits RetentionCopyLimits
	TTL    time.Duration
}

var (
	// ErrRetentionIneligible (EXPERIMENTAL) means the agent cannot certify a preview.
	ErrRetentionIneligible = errors.New("agent: retention preview ineligible")
	// ErrRetentionStale (EXPERIMENTAL) means the captured version no longer matches.
	ErrRetentionStale = errors.New("agent: retention snapshot stale")
	// ErrRetentionClosed (EXPERIMENTAL) includes closed, consumed and expired handles.
	ErrRetentionClosed = errors.New("agent: retention snapshot closed or expired")
)

type retentionOwnership struct {
	copier  *retentioncopy.Copier
	ttl     time.Duration
	limits  RetentionCopyLimits
	invalid bool
	handle  *RetentionSnapshot
}

// WithRetentionPreview (EXPERIMENTAL) enables owned ingress and detached outputs
// for a fresh agent. It requires immutable host configuration and host-controlled
// tools, models and middleware: plugins must not mutate retained requests/state
// or share concurrently mutable values. These are conditional ownership
// guarantees, not isolation from arbitrary plugins. WithState/restore is
// ineligible regardless of option order. Unsupported output values permanently
// disable capture before aliased data can be certified. Existing execution can
// continue. No classifier, automatic preview or compaction is installed.
func WithRetentionPreview(cfg RetentionPreviewConfig) (AgentOption, error) {
	c, err := retentioncopy.New(cfg.Limits)
	if err != nil {
		return nil, err
	}
	if cfg.TTL <= 0 || cfg.TTL > time.Minute {
		return nil, ErrRetentionIneligible
	}
	return func(a *UnifiedAgent) {
		a.retentionTracking = true
		a.retentionOwned = &retentionOwnership{copier: c, ttl: cfg.TTL, limits: cfg.Limits}
	}, nil
}

// RetentionSnapshotView (EXPERIMENTAL) contains independent mutable storage.
// It is full original context, not the post-middleware provider request.
type RetentionSnapshotView struct {
	Messages     []*message.Msg
	Summary      string
	SystemPrompt string
	Plan         []tool.PlanItem
}

// RetentionSnapshot (EXPERIMENTAL) is an opaque, agent-bound read-only handle.
// Methods serialize with writers through the owning agent. The zero value is
// closed. A view cannot be used as an application/compaction authorization.
type RetentionSnapshot struct {
	owner   *UnifiedAgent
	version retentionVersion
	expires time.Time
	data    []any
	closed  bool
}

// RetentionReceipt (EXPERIMENTAL) describes a validation linearization point.
// State may change immediately afterwards, before this receipt is delivered.
// It is not an apply token and does not assert validity at delivery time.
type RetentionReceipt struct {
	ValidatedAt time.Time
	Version     uint64
}

func (a *UnifiedAgent) retentionEligibleLocked() bool {
	if a.retentionOwned == nil || a.retentionOwned.invalid || a.retentionRestored || !a.retentionIdleLocked() {
		return false
	}
	if a.forkCaptureOn || a.contentBlockCompaction || a.state.PeerSummary != nil || a.state.ThreadCursor != 0 || a.boundaryInput != nil || a.threadSummarySet {
		return false
	}
	return a.contextCfg == nil || (a.contextCfg.PrecompressRatio == 0 && a.contextCfg.CompactStrategy != CompactStrategyFreshWindow && a.contextCfg.ThreadSummaryTokens == 0)
}

// retentionDataLocked produces only the closed set accepted by the copier.
// Plan expansion is checked before allocating; remaining copy work is bounded
// by the construction-time limits. Host/plugin configuration is immutable by
// contract; core configuration and complete message content are compared too.
func (a *UnifiedAgent) retentionDataLocked(ctx context.Context) ([]any, error) {
	o := a.retentionOwned
	if int64(len(a.state.Plan)) > o.limits.MaxNodes/2 || int64(len(a.state.Plan)) > o.limits.MaxAllocBytes/64 {
		return nil, retentioncopy.ErrLimit
	}
	plan := make([]string, 0, len(a.state.Plan)*2)
	for _, p := range a.state.Plan {
		plan = append(plan, p.Step, p.Status)
	}
	var cfg any
	if c := a.contextCfg; c != nil {
		cfg = []any{c.TriggerRatio, c.ReserveRatio, c.ContextSize, c.CompressionPrompt, c.SummarySchema, c.SummaryTemplate, c.AgentDrivenTriggerRatio, c.SummaryMaxTokens, c.ThreadSummaryTokens, c.CompactStrategy, c.PrecompressRatio}
	}
	limits := o.limits
	limits.MaxAllocBytes -= int64(len(a.state.Plan)) * 64
	if limits.MaxAllocBytes <= 0 {
		return nil, retentioncopy.ErrLimit
	}
	copier, err := retentioncopy.New(limits)
	if err != nil {
		return nil, err
	}
	v, _, err := copier.Copy(ctx, []any{a.state.Context, a.state.Summary, a.systemPrompt, plan, cfg, a.lastModelInputTokens})
	if err != nil {
		return nil, err
	}
	return v.([]any), nil
}

// CaptureRetentionSnapshot (EXPERIMENTAL) captures a bounded copy while idle.
// A live handle occupies the single slot until Close, Finish or lazy expiry.
// No callbacks, token counters or I/O run under the agent lock.
func (a *UnifiedAgent) CaptureRetentionSnapshot(ctx context.Context) (*RetentionSnapshot, error) {
	if ctx == nil {
		return nil, retentioncopy.ErrInvalidContext
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !a.retentionEligibleLocked() {
		return nil, ErrRetentionIneligible
	}
	o := a.retentionOwned
	if h := o.handle; h != nil {
		if time.Now().Before(h.expires) && !h.closed {
			return nil, ErrRetentionIneligible
		}
		h.releaseLocked()
	}
	data, err := a.retentionDataLocked(ctx)
	if err != nil {
		return nil, err
	}
	h := &RetentionSnapshot{owner: a, version: a.retentionVersion, expires: time.Now().Add(o.ttl), data: data}
	o.handle = h
	return h, nil
}

func (h *RetentionSnapshot) releaseLocked() {
	h.closed = true
	h.data = nil
	if h.owner.retentionOwned.handle == h {
		h.owner.retentionOwned.handle = nil
	}
}

func (h *RetentionSnapshot) checkLocked(ctx context.Context) error {
	if ctx == nil {
		return retentioncopy.ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.closed || h.owner.retentionOwned.handle != h {
		return ErrRetentionClosed
	}
	if !time.Now().Before(h.expires) {
		h.releaseLocked()
		return ErrRetentionClosed
	}
	if !h.owner.retentionEligibleLocked() || h.version != h.owner.retentionVersion {
		return ErrRetentionStale
	}
	data, err := h.owner.retentionDataLocked(ctx)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(data, h.data) {
		return ErrRetentionStale
	}
	if !time.Now().Before(h.expires) {
		h.releaseLocked()
		return ErrRetentionClosed
	}
	return ctx.Err()
}

// Validate (EXPERIMENTAL) checks ownership, expiry, writers, epoch and complete
// copied content/configuration. It neither consumes nor applies the handle.
func (h *RetentionSnapshot) Validate(ctx context.Context) error {
	if h == nil || h.owner == nil {
		return ErrRetentionClosed
	}
	h.owner.mu.Lock()
	defer h.owner.mu.Unlock()
	return h.checkLocked(ctx)
}

// View (EXPERIMENTAL) returns a fresh bounded copy of a still-valid snapshot.
func (h *RetentionSnapshot) View(ctx context.Context) (RetentionSnapshotView, error) {
	if h == nil || h.owner == nil {
		return RetentionSnapshotView{}, ErrRetentionClosed
	}
	h.owner.mu.Lock()
	defer h.owner.mu.Unlock()
	if err := h.checkLocked(ctx); err != nil {
		return RetentionSnapshotView{}, err
	}
	limits := h.owner.retentionOwned.limits
	limits.MaxAllocBytes -= int64(len(h.data[3].([]string))) * 32
	if limits.MaxAllocBytes <= 0 {
		return RetentionSnapshotView{}, retentioncopy.ErrLimit
	}
	copier, err := retentioncopy.New(limits)
	if err != nil {
		return RetentionSnapshotView{}, err
	}
	v, _, err := copier.Copy(ctx, h.data)
	if err != nil {
		return RetentionSnapshotView{}, err
	}
	if !time.Now().Before(h.expires) {
		h.releaseLocked()
		return RetentionSnapshotView{}, ErrRetentionClosed
	}
	data := v.([]any)
	p := data[3].([]string)
	plan := make([]tool.PlanItem, 0, len(p)/2)
	for i := 0; i < len(p); i += 2 {
		plan = append(plan, tool.PlanItem{Step: p[i], Status: p[i+1]})
	}
	return RetentionSnapshotView{Messages: data[0].([]*message.Msg), Summary: data[1].(string), SystemPrompt: data[2].(string), Plan: plan}, nil
}

// Finish (EXPERIMENTAL) validates once and consumes the handle on every attempt,
// including failure. The receipt is valid-as-of only and cannot be applied.
func (h *RetentionSnapshot) Finish(ctx context.Context) (RetentionReceipt, error) {
	if h == nil || h.owner == nil {
		return RetentionReceipt{}, ErrRetentionClosed
	}
	h.owner.mu.Lock()
	defer h.owner.mu.Unlock()
	defer h.releaseLocked()
	if err := h.checkLocked(ctx); err != nil {
		return RetentionReceipt{}, err
	}
	return RetentionReceipt{ValidatedAt: time.Now(), Version: h.version.epoch}, nil
}

// Close (EXPERIMENTAL) releases copied content and the slot; it is idempotent.
func (h *RetentionSnapshot) Close() {
	if h == nil || h.owner == nil {
		return
	}
	h.owner.mu.Lock()
	defer h.owner.mu.Unlock()
	h.releaseLocked()
}

func (a *UnifiedAgent) retentionCopyLocked(ctx context.Context, input any) (any, error) {
	if a.retentionOwned == nil {
		return input, nil
	}
	out, _, err := a.retentionOwned.copier.Copy(ctx, input)
	return out, err
}

func (a *UnifiedAgent) ownRetentionUpdateLocked(ctx context.Context, u *ThreadUpdate) error {
	if a.retentionOwned == nil {
		return nil
	}
	v, err := a.retentionCopyLocked(ctx, u.Messages)
	if err != nil {
		return err
	}
	u.Messages = v.([]*message.Msg)
	u.PeerSummary = u.PeerSummary.clone()
	return nil
}
