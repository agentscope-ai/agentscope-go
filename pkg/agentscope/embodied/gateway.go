package embodied

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/fsutil"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/taskenvelope"
)

// GatewayConfig binds durable ledgers, a controller, skills and the host clock.
type GatewayConfig struct {
	LedgerPath   string
	MetadataPath string
	Controller   taskenvelope.Executor
	Skills       []Skill
	Now          func() time.Time
}

type gatewayMetadata struct {
	Actions       map[string]*Action      `json:"actions"`
	Observations  map[string]*Observation `json:"observations"`
	CanceledRoots map[string]bool         `json:"canceled_roots"`
	Epochs        map[string]int64        `json:"epochs"`
}

// Gateway owns device-side action admission while taskenvelope owns lifecycle state.
type Gateway struct {
	config        *GatewayConfig
	store         *taskenvelope.Store
	lifecycleMu   sync.RWMutex
	mu            sync.Mutex
	skills        map[string]Skill
	worlds        map[string]WorldState
	actions       map[string]*Action
	observations  map[string]*Observation
	canceledRoots map[string]bool
	epochs        map[string]int64
	recovering    atomic.Bool
}

// NewGateway loads persisted action bindings and opens the taskenvelope ledger.
func NewGateway(cfg *GatewayConfig) (*Gateway, error) {
	if cfg.LedgerPath == "" || cfg.MetadataPath == "" || cfg.Controller == nil || cfg.Now == nil {
		return nil, errors.New("embodied: invalid gateway configuration")
	}
	g := &Gateway{
		config: cfg, skills: make(map[string]Skill), worlds: make(map[string]WorldState),
		actions: make(map[string]*Action), observations: make(map[string]*Observation), canceledRoots: make(map[string]bool), epochs: make(map[string]int64),
	}
	for _, skill := range cfg.Skills {
		if skill.Name == "" || skill.Version == "" || len(skill.Destinations) == 0 {
			return nil, errors.New("embodied: invalid skill registration")
		}
		skill.Destinations = append([]string(nil), skill.Destinations...)
		skill.Preconditions = append([]string(nil), skill.Preconditions...)
		for _, destination := range skill.Destinations {
			scope := skill.Name + "@" + destination + "@" + skill.Version
			if destination == "" || g.skills[scope].Name != "" {
				return nil, errors.New("embodied: duplicate skill scope")
			}
			g.skills[scope] = skill
		}
	}
	if err := g.loadMetadata(); err != nil {
		return nil, err
	}
	store, err := taskenvelope.Open(cfg.LedgerPath, &taskenvelope.Options{Authority: g.scopeAllowed, Now: cfg.Now})
	if err != nil {
		return nil, err
	}
	g.store = store
	for root := range g.epochs {
		g.epochs[root] = store.Epoch(root)
	}
	for _, action := range g.actions {
		g.epochs[action.Plan.RootTaskID] = store.Epoch(action.Plan.RootTaskID)
	}
	if controller, ok := cfg.Controller.(*SimController); ok {
		controller.finalize = g.finalAdmission
	}
	return g, nil
}

// RegisterRoot declares a root task allowance pool.
func (g *Gateway) RegisterRoot(rootTaskID string, allowanceUnits int) error {
	g.lifecycleMu.Lock()
	defer g.lifecycleMu.Unlock()
	if err := g.store.RegisterRoot(rootTaskID, allowanceUnits); err != nil {
		return err
	}
	epoch := g.store.Epoch(rootTaskID)
	g.mu.Lock()
	g.epochs[rootTaskID] = epoch
	err := g.persistMetadataLocked()
	g.mu.Unlock()
	return err
}

// SetWorldState replaces the trusted current state for a root task.
func (g *Gateway) SetWorldState(state *WorldState) {
	g.mu.Lock()
	defer g.mu.Unlock()
	state.Conditions = cloneConditions(state.Conditions)
	g.worlds[state.RootTaskID] = *state
}

// RegisterObservation binds an immutable observation ID to its validated content.
func (g *Gateway) RegisterObservation(obs *Observation) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	obs = cloneObservation(obs)
	if err := g.validateObservationLocked(obs); err != nil {
		return err
	}
	hash := observationHash(obs)
	if existing, ok := g.observations[obs.ID]; ok {
		if observationHash(existing) != hash {
			return fmt.Errorf("%w: %s", ErrObservationConflict, obs.ID)
		}
		return nil
	}
	g.observations[obs.ID] = obs
	if err := g.persistMetadataLocked(); err != nil {
		delete(g.observations, obs.ID)
		return err
	}
	return nil
}

// Execute validates and dispatches one action without advancing any plan step.
func (g *Gateway) Execute(ctx context.Context, action *Action) (*taskenvelope.Record, error) {
	g.lifecycleMu.RLock()
	defer g.lifecycleMu.RUnlock()
	if g.recovering.Load() {
		return nil, errors.New("embodied: recovery in progress")
	}
	if err := g.bindAction(action); err != nil {
		rec, _ := g.ActionRecord(action.ID)
		return rec, err
	}
	content, err := action.canonicalContent()
	if err != nil {
		return nil, err
	}
	rec, err := g.store.Submit(&taskenvelope.Submission{
		ID: action.ID, RootTaskID: action.Plan.RootTaskID, ParentID: action.Plan.ID,
		Scope: action.Scope(), Content: content, Epoch: action.Plan.TaskEpoch,
		TTL: action.Plan.ValidUntil.Sub(g.config.Now()), MaxAttempts: 1, AllowanceUnits: 1,
	})
	if err != nil || rec.State != taskenvelope.StateAccepted {
		return rec, err
	}
	if err := g.store.Dispatch(ctx, action.ID, content, g.config.Controller); err != nil {
		rec, _ := g.ActionRecord(action.ID)
		return rec, err
	}
	rec, _ = g.ActionRecord(action.ID)
	return rec, nil
}

func (g *Gateway) bindAction(action *Action) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	action = cloneAction(action)
	if existing, ok := g.actions[action.ID]; ok && canonicalHash(existing) != canonicalHash(action) {
		return fmt.Errorf("%w: %s", ErrActionConflict, action.ID)
	}
	if err := g.validateActionLocked(action); err != nil {
		return err
	}
	if _, ok := g.actions[action.ID]; ok {
		return nil
	}
	g.actions[action.ID] = action
	if err := g.persistMetadataLocked(); err != nil {
		delete(g.actions, action.ID)
		return err
	}
	return nil
}

func (g *Gateway) validateActionLocked(action *Action) error {
	if g.canceledRoots[action.Plan.RootTaskID] || g.epochs[action.Plan.RootTaskID] != action.Plan.TaskEpoch {
		return ErrStalePlan
	}
	if err := action.ValidateBasic(g.config.Now()); err != nil {
		return err
	}
	if err := g.validateObservationLocked(&action.Observation); err != nil {
		return err
	}
	registered, ok := g.observations[action.Observation.ID]
	if !ok || observationHash(registered) != observationHash(&action.Observation) {
		return fmt.Errorf("%w: observation identity mismatch", ErrStalePlan)
	}
	if _, ok := g.skills[action.Scope()]; !ok {
		return fmt.Errorf("%w: skill or destination revoked", ErrStalePlan)
	}
	world := g.worlds[action.Plan.RootTaskID]
	for _, condition := range append(g.skills[action.Scope()].Preconditions, action.Plan.Steps[action.StepIndex].Preconditions...) {
		if !world.Conditions[condition] {
			return fmt.Errorf("%w: precondition %s", ErrStalePlan, condition)
		}
	}
	return nil
}

func (g *Gateway) validateObservationLocked(obs *Observation) error {
	world, ok := g.worlds[obs.RootTaskID]
	if !ok {
		return ErrStaleObservation
	}
	return obs.ValidateAt(g.config.Now(), &world)
}

func (g *Gateway) scopeAllowed(scope string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.skills[scope]
	return ok
}

func (g *Gateway) finalAdmission(attempt taskenvelope.Attempt) error {
	var action *Action
	if err := json.Unmarshal([]byte(attempt.Content), &action); err != nil {
		return fmt.Errorf("%w: invalid action payload", ErrControllerIntegrity)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.validateActionLocked(action)
}

// Recover reconciles persisted records and resolves uncertainty only from controller evidence.
func (g *Gateway) Recover(ctx context.Context) error {
	g.lifecycleMu.Lock()
	defer g.lifecycleMu.Unlock()
	if !g.recovering.CompareAndSwap(false, true) {
		return errors.New("embodied: recovery already in progress")
	}
	defer g.recovering.Store(false)
	staleRoots := make(map[string]bool)
	g.mu.Lock()
	actionIDs := make([]string, 0, len(g.actions))
	for id, action := range g.actions {
		actionIDs = append(actionIDs, id)
		if err := g.validateActionLocked(action); err != nil {
			staleRoots[action.Plan.RootTaskID] = true
		}
	}
	g.mu.Unlock()
	for root := range staleRoots {
		if err := g.store.CancelRoot(root); err != nil {
			return err
		}
	}
	if err := g.store.Reconcile(ctx, g.config.Controller); err != nil {
		return err
	}
	for _, actionID := range actionIDs {
		rec, ok := g.ActionRecord(actionID)
		if !ok {
			continue
		}
		if rec.State == taskenvelope.StateCanceling {
			if err := g.store.ProcessCancel(ctx, actionID, g.config.Controller); err != nil {
				return err
			}
			rec, _ = g.ActionRecord(actionID)
		}
		if rec.State != taskenvelope.StateUnknownEffect || len(rec.Attempts) == 0 {
			continue
		}
		attemptID := rec.Attempts[len(rec.Attempts)-1].ID
		outcome, accounted, err := g.config.Controller.QueryAttempt(ctx, attemptID)
		if err != nil {
			return err
		}
		if !accounted {
			continue
		}
		state := taskenvelope.StateFailed
		if outcome.Success {
			state = taskenvelope.StateCompleted
		}
		if err := g.store.Resolve(actionID, &taskenvelope.Evidence{
			Outcome: state, Result: outcome.Result, Usage: outcome.Usage, Effect: outcome.Effect,
			Provenance: "controller query " + attemptID, Validated: true,
		}); err != nil {
			return err
		}
	}
	return nil
}

// CancelRoot durably cancels a root and prevents stale replay from authorizing work.
func (g *Gateway) CancelRoot(rootTaskID string) error {
	g.mu.Lock()
	g.canceledRoots[rootTaskID] = true
	err := g.persistMetadataLocked()
	g.mu.Unlock()
	if err != nil {
		return err
	}
	return g.store.CancelRoot(rootTaskID)
}

// AdvanceEpoch invalidates older root records according to taskenvelope semantics.
func (g *Gateway) AdvanceEpoch(rootTaskID string) (int64, error) {
	g.lifecycleMu.Lock()
	defer g.lifecycleMu.Unlock()
	epoch, err := g.store.AdvanceEpoch(rootTaskID)
	if err != nil {
		return 0, err
	}
	g.mu.Lock()
	g.epochs[rootTaskID] = epoch
	err = g.persistMetadataLocked()
	g.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return epoch, nil
}

// ActionRecord returns historical evidence without treating it as current authorization.
func (g *Gateway) ActionRecord(actionID string) (*taskenvelope.Record, bool) {
	return g.store.Get(actionID)
}

func (g *Gateway) loadMetadata() error {
	data, err := os.ReadFile(g.config.MetadataPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var metadata gatewayMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return fmt.Errorf("embodied: decode metadata: %w", err)
	}
	for id, action := range metadata.Actions {
		g.actions[id] = action
	}
	for id, obs := range metadata.Observations {
		g.observations[id] = obs
	}
	for root, canceled := range metadata.CanceledRoots {
		g.canceledRoots[root] = canceled
	}
	for root, epoch := range metadata.Epochs {
		g.epochs[root] = epoch
	}
	return nil
}

func (g *Gateway) persistMetadataLocked() error {
	wire, err := json.Marshal(gatewayMetadata{Actions: g.actions, Observations: g.observations, CanceledRoots: g.canceledRoots, Epochs: g.epochs})
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(g.config.MetadataPath, wire, 0o600)
}

func cloneConditions(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
