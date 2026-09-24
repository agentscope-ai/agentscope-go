// Package inference provides opt-in, single-process admission and physical HTTP
// attempt accounting for explicitly supported model and embedding adapters.
package inference

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	ErrQueueFull        = errors.New("inference: admission queue is full")
	ErrClosed           = errors.New("inference: controller is closed")
	ErrIdentityRequired = errors.New("inference: trusted tenant identity is required")
	ErrAttemptLimit     = errors.New("inference: operation attempt limit reached")
)

// Descriptor identifies one configured target, independently of its shared pool.
// Endpoint is a credential-free base URL; model cards do not establish its limits.
type Descriptor struct {
	ID          string `json:"id"`
	Provider    string `json:"provider"`
	API         string `json:"api"`
	Model       string `json:"model"`
	Endpoint    string `json:"endpoint"`
	ContextSize int    `json:"context_size,omitempty"`
}

// PoolConfig bounds active physical requests and FIFO queued demand. A zero
// MaxQueued rejects excess demand immediately. Work units are host estimates.
type PoolConfig struct {
	ID            string
	MaxActive     int
	MaxQueued     int
	MaxQueuedWork int64
}

// Price contains explicit USD rates per million tokens. Nil means unknown;
// a non-nil zero is an explicitly free category. Input excludes cache tokens.
type Price struct {
	Input      *float64
	Output     *float64
	CacheRead  *float64
	CacheWrite *float64
}

// DeploymentConfig binds an immutable target to a pre-registered pool.
// MaxAttempts is the total send cap per operation, including shape fallbacks.
// Backoff is the exponential base (zero selects 200ms); Retry-After is a floor.
type DeploymentConfig struct {
	Descriptor  Descriptor
	PoolID      string
	MaxAttempts int
	Backoff     time.Duration
	Price       *Price
}

// Controller is a host-owned registry. Share it across agents and callers.
// Close rejects queued/new work and cancels active HTTP requests. It does not
// establish that remote computation has stopped, or wait for client consumers.
type Controller struct {
	mu          sync.Mutex
	pools       map[string]*pool
	deployments map[string]*Deployment
	ledger      *Ledger
	closed      bool
}

func NewController(ledger *Ledger) *Controller {
	if ledger == nil {
		ledger = NewLedger(10000)
	}
	return &Controller{pools: make(map[string]*pool), deployments: make(map[string]*Deployment), ledger: ledger}
}

func (c *Controller) RegisterPool(cfg PoolConfig) error {
	if strings.TrimSpace(cfg.ID) == "" || cfg.MaxActive <= 0 || cfg.MaxQueued < 0 || cfg.MaxQueuedWork < 0 || (cfg.MaxQueued > 0 && cfg.MaxQueuedWork == 0) {
		return fmt.Errorf("inference: invalid pool limits")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if _, ok := c.pools[cfg.ID]; ok {
		return fmt.Errorf("inference: duplicate pool %q", cfg.ID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.pools[cfg.ID] = &pool{cfg: cfg, ctx: ctx, cancel: cancel}
	return nil
}

func (c *Controller) Register(config *DeploymentConfig) (*Deployment, error) {
	if config == nil {
		return nil, fmt.Errorf("inference: nil deployment configuration")
	}
	cfg := *config
	desc := cfg.Descriptor
	u, err := url.Parse(desc.Endpoint)
	if err != nil || u == nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(desc.ID) == "" || strings.TrimSpace(desc.Provider) == "" || strings.TrimSpace(desc.API) == "" || strings.TrimSpace(desc.Model) == "" || desc.ContextSize < 0 || cfg.MaxAttempts < 1 || cfg.MaxAttempts > 100 || cfg.Backoff < 0 || cfg.Backoff > 30*time.Second {
		return nil, fmt.Errorf("inference: invalid deployment configuration")
	}
	cfg.Descriptor.Endpoint = strings.TrimRight(desc.Endpoint, "/")
	if cfg.Backoff == 0 {
		cfg.Backoff = 200 * time.Millisecond
	}
	if cfg.Price != nil {
		p := *cfg.Price
		rates := []**float64{&p.Input, &p.Output, &p.CacheRead, &p.CacheWrite}
		for _, rate := range rates {
			if *rate != nil {
				v := **rate
				if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
					return nil, fmt.Errorf("inference: invalid price")
				}
				*rate = &v
			}
		}
		cfg.Price = &p
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if _, ok := c.deployments[desc.ID]; ok {
		return nil, fmt.Errorf("inference: duplicate deployment %q", desc.ID)
	}
	p, ok := c.pools[cfg.PoolID]
	if !ok {
		return nil, fmt.Errorf("inference: unknown admission pool %q", cfg.PoolID)
	}
	d := &Deployment{cfg: cfg, pool: p, ledger: c.ledger}
	c.deployments[desc.ID] = d
	return d, nil
}

func (c *Controller) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	for _, p := range c.pools {
		p.close()
	}
}

// Deployment is immutable and safe to share. It does not make an arbitrary
// adapter concurrency-safe; managed constructors only accept audited adapters.
type Deployment struct {
	cfg    DeploymentConfig
	pool   *pool
	ledger *Ledger
}

func (d *Deployment) Descriptor() Descriptor { return d.cfg.Descriptor }
func (d *Deployment) PoolID() string         { return d.cfg.PoolID }
func (d *Deployment) Stats() Stats           { return d.pool.stats() }
func (d *Deployment) acquire(ctx context.Context, work int64) (func(), error) {
	return d.pool.acquire(ctx, work)
}
func (d *Deployment) close() { d.pool.close() }

// ValidateBinding prevents a host label from silently describing another model
// or endpoint. Provider labels can describe compatible third-party deployments;
// API, model and endpoint must match the concrete adapter's actual request path.
func (d *Deployment) ValidateBinding(api, model, endpoint string) error {
	if d == nil {
		return fmt.Errorf("inference: nil deployment")
	}
	if d.cfg.Descriptor.API != api || d.cfg.Descriptor.Model != model || d.cfg.Descriptor.Endpoint != strings.TrimRight(endpoint, "/") {
		return fmt.Errorf("inference: adapter does not match deployment %q", d.cfg.Descriptor.ID)
	}
	return nil
}

// Identity must be supplied by trusted host code after authenticating a request.
// These values are never inferred from prompts or model output.
type Identity struct {
	TenantID  string `json:"tenant_id"`
	SessionID string `json:"session_id,omitempty"`
	AgentName string `json:"agent_name,omitempty"`
}

// Attribution joins an attempt to a scheduled task. Iteration and Repeat are one-based.
type Attribution struct {
	RunID     string `json:"run_id,omitempty"`
	Scenario  string `json:"scenario,omitempty"`
	Iteration int    `json:"iteration,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	Repeat    int    `json:"repeat,omitempty"`
}
type identityKey struct{}
type attributionKey struct{}
type purposeKey struct{}
type workKey struct{}

func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}
func IdentityFromContext(ctx context.Context) Identity {
	id, _ := ctx.Value(identityKey{}).(Identity)
	return id
}
func WithAttribution(ctx context.Context, a Attribution) context.Context {
	return context.WithValue(ctx, attributionKey{}, a)
}
func AttributionFromContext(ctx context.Context) Attribution {
	a, _ := ctx.Value(attributionKey{}).(Attribution)
	return a
}
func WithPurpose(ctx context.Context, purpose string) context.Context {
	return context.WithValue(ctx, purposeKey{}, purpose)
}
func PurposeFromContext(ctx context.Context) string {
	s, _ := ctx.Value(purposeKey{}).(string)
	return s
}

// WithEstimatedWork sets queued demand units for calls made with ctx. Estimates
// do not reserve money or provide a strict token/window bound. Values < 1 use 1.
func WithEstimatedWork(ctx context.Context, work int64) context.Context {
	return context.WithValue(ctx, workKey{}, work)
}
func estimatedWork(ctx context.Context) int64 {
	w, _ := ctx.Value(workKey{}).(int64)
	if w < 1 {
		return 1
	}
	return w
}

// Ledger returns the canonical attempt source for this deployment.
func (d *Deployment) Ledger() *Ledger { return d.ledger }
