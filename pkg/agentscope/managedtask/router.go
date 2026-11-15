package managedtask

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"maps"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

// Rule is trusted, concurrency-safe local host code. Empty category is a miss;
// an error or unknown nonempty category requires the explicit default route.
// Rules must honor cancellation and must not make hidden external requests.
type Rule func(context.Context, Task) (category string, err error)

// WeightedDeployment assigns a positive integer weight to a stable deployment ID.
type WeightedDeployment struct {
	ID     string
	Weight uint64
}

// Pool selects among qualified Members, then tries explicit Fallback IDs in order.
// AttemptsPerDeployment defaults to one; all calls share the task attempt cap.
// There are no implicit fallbacks or automatic quality-based escalations.
type Pool struct {
	ID                    string
	Members               []WeightedDeployment
	Fallback              []string
	AttemptsPerDeployment int
}

// DefaultRoute explicitly binds an otherwise undecidable task to a category and
// pool. Deployment qualification and current host authorization still apply.
type DefaultRoute struct{ Category, PoolID string }

// Route records only configured identifiers. Source is metadata, rule,
// classifier or default. DeploymentID is the last candidate examined.
type Route struct{ Source, Category, PoolID, DeploymentID string }

// RouterConfig registers fixed PR3 configurations and host policy. Deployment
// Limits must be nil: all routes use these shared Limits (nil selects defaults).
// Categories maps trusted category names to pools. Unknown metadata does not
// fall through to rules/classification. Strict monetary budgets are unsupported.
// Configuration is copied; host callbacks and clients remain shared trusted code.
type RouterConfig struct {
	Deployments    []Config
	Pools          []Pool
	Categories     map[string]string
	Default        *DefaultRoute
	Rules          []Rule
	Classification *ClassificationConfig
	Limits         *Limits
	StrictBudget   *float64
}

// ClassificationTarget binds disclosure policy to a classifier endpoint/version.
type ClassificationTarget struct{ ID, Endpoint, Model string }

// ChoiceDecision binds a confidence threshold to an exact question (including
// instructions and candidate descriptions) and the configured classifier version.
// Categories maps every choice to a category; empty means insufficient information.
// All decisions must agree. Marginals are never combined into joint confidence.
type ChoiceDecision struct {
	QuestionID    string
	Question      classifier.ChoiceQuestion
	MinConfidence float64
	Categories    map[string]string
}

// ClassificationConfig opts into one logical operation per task. Project runs
// after Authorize and must filter BOTH State and every question field; it must
// return exactly the configured questions. OperationID is assigned by Router.
// Authorize runs again at each dispatch and must reject stale projected data.
// Prices are optional per-token classifier prices; cached pricing is unsupported.
// MaxAttempts bounds primitive calls independently of the shared task cap.
type ClassificationConfig struct {
	Client      classifier.DispatchClassifier
	Target      ClassificationTarget
	MaxAttempts int
	Authorize   func(context.Context, Task, ClassificationTarget) error
	Project     func(context.Context, Task, ClassificationTarget) (classifier.Request, error)
	Decisions   []ChoiceDecision
	Prices      *Prices
}

// Router owns an immutable registry with task, queue and active-request capacity
// shared across classification and all generation deployments. Construct with
// NewRouter and share the instance across callers. It never mutates an Agent.
type Router struct {
	deployments    map[string]*Executor
	pools          map[string]Pool
	categories     map[string]string
	fallback       *DefaultRoute
	rules          []Rule
	classification *ClassificationConfig
	limits         Limits
	tasks          chan struct{}
	admission      *admission
}

// ErrRoute means no allowed, sufficiently supported route could be established.
var ErrRoute = &ae.AgentError{Category: ae.CategoryPermission, Code: "managedtask.route", Message: "managed task: no allowed route"}

// NewRouter validates and snapshots all registrations without host callbacks or IO.
func NewRouter(cfg RouterConfig) (*Router, error) { //nolint:gocritic // explicit configuration value
	if len(cfg.Deployments) == 0 || len(cfg.Pools) == 0 || len(cfg.Categories) == 0 || cfg.StrictBudget != nil {
		return nil, ErrConfig
	}
	r := &Router{deployments: make(map[string]*Executor), pools: make(map[string]Pool), categories: maps.Clone(cfg.Categories), rules: append([]Rule(nil), cfg.Rules...)}
	currency := ""
	for i := range cfg.Deployments {
		c := cfg.Deployments[i]
		if c.Limits != nil {
			return nil, ErrConfig
		}
		c.Limits = cfg.Limits
		e, err := New(c)
		if err != nil {
			return nil, err
		}
		id := e.cfg.Deployment.ID
		if r.deployments[id] != nil {
			return nil, ErrConfig
		}
		r.deployments[id] = e
		r.limits = e.limits
		if e.cfg.Prices != nil {
			if currency != "" && currency != e.cfg.Prices.Currency {
				return nil, ErrConfig
			}
			currency = e.cfg.Prices.Currency
		}
	}
	r.tasks = make(chan struct{}, r.limits.MaxTasks)
	r.admission = &admission{maxActive: r.limits.MaxConcurrent, maxQueued: r.limits.MaxQueued}
	for _, e := range r.deployments {
		e.admission = r.admission
	}
	for _, p := range cfg.Pools {
		if p.AttemptsPerDeployment == 0 {
			p.AttemptsPerDeployment = 1
		}
		if p.AttemptsPerDeployment < 0 || p.AttemptsPerDeployment > r.limits.MaxAttempts {
			return nil, ErrConfig
		}
		if strings.TrimSpace(p.ID) == "" || len(p.Members) == 0 {
			return nil, ErrConfig
		}
		if _, ok := r.pools[p.ID]; ok {
			return nil, ErrConfig
		}
		seen := map[string]bool{}
		var total uint64
		for _, m := range p.Members {
			if r.deployments[m.ID] == nil || seen[m.ID] || m.Weight == 0 || math.MaxUint64-total < m.Weight {
				return nil, ErrConfig
			}
			seen[m.ID] = true
			total += m.Weight
		}
		seen = map[string]bool{}
		for _, id := range p.Fallback {
			if r.deployments[id] == nil || seen[id] {
				return nil, ErrConfig
			}
			seen[id] = true
		}
		p.Members = append([]WeightedDeployment(nil), p.Members...)
		p.Fallback = append([]string(nil), p.Fallback...)
		r.pools[p.ID] = p
	}
	for category, pool := range r.categories {
		if strings.TrimSpace(category) == "" {
			return nil, ErrConfig
		}
		if _, ok := r.pools[pool]; !ok {
			return nil, ErrConfig
		}
	}
	if cfg.Default != nil {
		d := *cfg.Default
		if d.Category == "" || r.categories[d.Category] != d.PoolID {
			return nil, ErrConfig
		}
		r.fallback = &d
	}
	for _, rule := range r.rules {
		if rule == nil {
			return nil, ErrConfig
		}
	}
	if cfg.Classification != nil {
		c, err := r.snapshotClassification(cfg.Classification)
		if err != nil {
			return nil, err
		}
		r.classification = c
		if c.Prices != nil && currency != "" && currency != c.Prices.Currency {
			return nil, ErrConfig
		}
	}
	return r, nil
}

// Execute returns one frozen terminal result within the shared task deadline.
// Empty Category enables rules and optional classification; nonempty Category is
// trusted host metadata. Cleanup retains capacity until local work actually ends.
func (r *Router) Execute(parent context.Context, task Task) *Result {
	if r == nil {
		return &Result{TaskID: task.ID, Status: Rejected, Err: ErrConfig}
	}
	if parent == nil {
		return &Result{TaskID: task.ID, Status: Rejected, Err: ErrTask}
	}
	ctx, cancel := context.WithTimeout(parent, r.limits.TaskTimeout)
	defer cancel()
	state := &execution{result: Result{TaskID: task.ID}, ctx: ctx, cost: r.cost}
	if ctx.Err() != nil {
		return state.finish(Canceled, "", ctx.Err())
	}
	if task.Mode == "" {
		task.Mode = Production
	}
	if strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.Text) == "" || !utf8.ValidString(task.Text) || len(task.Text) > r.limits.MaxRequestBytes || (task.Mode != Production && task.Mode != Evaluation) {
		return state.finish(Rejected, "", ErrTask)
	}
	select {
	case r.tasks <- struct{}{}:
	default:
		return state.finish(Rejected, "", ErrCapacity)
	}
	done := make(chan struct{})
	go func() { defer func() { <-r.tasks; close(done) }(); r.run(ctx, task, state) }()
	select {
	case <-done:
		return state.snapshot()
	case <-ctx.Done():
		return state.finish(Canceled, "", ctx.Err())
	}
}

func (r *Router) decide(ctx context.Context, task Task, state *execution) (Route, error) {
	source, category := "metadata", task.Category
	if category == "" {
		source = "rule"
		for _, rule := range r.rules {
			var err error
			category, err = rule(ctx, task)
			if ctx.Err() != nil {
				return Route{}, ctx.Err()
			}
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return Route{}, hostError(ctx, err, ErrRoute)
				}
				return r.defaultRoute()
			}
			if category != "" {
				break
			}
		}
		if category == "" && r.classification != nil {
			source = "classifier"
			var err error
			category, err = r.classify(ctx, task, state)
			if ctx.Err() != nil {
				return Route{}, ctx.Err()
			}
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return Route{}, err
				}
				return r.defaultRoute()
			}
		}
	}
	pool, ok := r.categories[category]
	if !ok {
		return r.defaultRoute()
	}
	return Route{Source: source, Category: category, PoolID: pool}, nil
}
func (r *Router) defaultRoute() (Route, error) {
	if r.fallback == nil {
		return Route{}, ErrRoute
	}
	return Route{Source: "default", Category: r.fallback.Category, PoolID: r.fallback.PoolID}, nil
}

func (r *Router) run(ctx context.Context, task Task, state *execution) {
	route, err := r.decide(ctx, task, state)
	if err != nil {
		state.finish(Rejected, "", err)
		return
	}
	state.mu.Lock()
	if !state.terminal {
		state.result.Route = route
	}
	state.mu.Unlock()
	task.Category = route.Category
	p := r.pools[route.PoolID]
	candidates := r.candidates(task, p)
	status, text, lastErr := Rejected, "", error(ErrUnqualified)
	for _, id := range candidates {
		if ctx.Err() != nil {
			state.finish(Canceled, "", ctx.Err())
			return
		}
		if !state.remaining(r.limits.MaxAttempts) {
			lastErr = ErrCapacity
			status = Rejected
			break
		}
		route.DeploymentID = id
		state.mu.Lock()
		if !state.terminal {
			state.result.Route = route
		}
		state.mu.Unlock()
		status, text, lastErr = r.deployments[id].runAttempts(ctx, task, state, p.AttemptsPerDeployment)
		if status == Succeeded || status == Canceled || status == TimedOut || status == Unsupported || status == Filtered || status == Truncated {
			break
		}
	}
	state.finish(status, text, lastErr)
}

func (r *Router) candidates(task Task, p Pool) []string { //nolint:gocritic // immutable pool value
	var total uint64
	eligible := make([]WeightedDeployment, 0, len(p.Members))
	for _, m := range p.Members {
		if r.deployments[m.ID].qualified(task) {
			eligible = append(eligible, m)
			total += m.Weight
		}
	}
	out := make([]string, 0, 1+len(p.Fallback))
	if total > 0 {
		hash := sha256.Sum256([]byte(task.ID + "\x00" + p.ID))
		slot := binary.BigEndian.Uint64(hash[:8]) % total
		for _, m := range eligible {
			if slot < m.Weight {
				out = append(out, m.ID)
				break
			}
			slot -= m.Weight
		}
	}
	for _, id := range p.Fallback {
		if len(out) == 0 || out[0] != id {
			out = append(out, id)
		}
	}
	return out
}

func (r *Router) cost(attempts []Attempt) Cost {
	total := Cost{}
	for i := range attempts {
		a := &attempts[i]
		if a.Kind == SimulatedAttempt {
			continue
		}
		e := r.deployments[a.DeploymentID]
		if a.OperationID != "" && r.classification != nil {
			e = &Executor{cfg: Config{Deployment: Deployment{Model: r.classification.Target.Model}, Prices: r.classification.Prices}}
		}
		c := e.cost(attempts[i : i+1])
		if c.Currency != "" {
			total.Currency = c.Currency
		}
		total.UnknownAttempts += c.UnknownAttempts
		if finite(total.KnownSubtotal + c.KnownSubtotal) {
			total.KnownSubtotal += c.KnownSubtotal
		} else if c.UnknownAttempts == 0 {
			total.UnknownAttempts++
		}
	}
	return total
}
