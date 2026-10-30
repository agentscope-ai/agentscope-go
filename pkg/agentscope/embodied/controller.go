package embodied

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/fsutil"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/taskenvelope"
)

// ControllerRecord is independent durable evidence of one simulated command and effect.
type ControllerRecord struct {
	AttemptID     string                `json:"attempt_id"`
	PayloadHash   string                `json:"payload_hash"`
	Command       string                `json:"command"`
	Effect        taskenvelope.Effect   `json:"effect"`
	EffectApplied bool                  `json:"effect_applied"`
	Outcome       *taskenvelope.Outcome `json:"outcome,omitempty"`
}

// SimController is a deterministic controller with final admission and a durable effect log.
type SimController struct {
	mu       sync.Mutex
	path     string
	finalize func(taskenvelope.Attempt) error
	records  map[string]*ControllerRecord

	LoseACK         bool
	FailPartial     bool
	Gate            chan struct{}
	BeforeAdmission func()
	AfterEffect     func()
}

var _ taskenvelope.Executor = (*SimController)(nil)

// OpenSimController loads the complete durable controller log.
func OpenSimController(path string, finalize func(taskenvelope.Attempt) error) (*SimController, error) {
	controller := &SimController{path: path, finalize: finalize, records: make(map[string]*ControllerRecord)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return controller, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &controller.records); err != nil {
		return nil, fmt.Errorf("embodied: decode controller log: %w", err)
	}
	return controller, nil
}

// Do performs final admission and applies at most one effect for an attempt ID.
func (c *SimController) Do(ctx context.Context, attempt taskenvelope.Attempt) (taskenvelope.Outcome, error) {
	if c.Gate != nil {
		select {
		case <-c.Gate:
		case <-ctx.Done():
			return taskenvelope.Outcome{}, ctx.Err()
		}
	}
	payloadHash := canonicalHash(attempt)
	c.mu.Lock()
	defer c.mu.Unlock()
	if rec, ok := c.records[attempt.ID]; ok {
		if rec.PayloadHash != payloadHash {
			return taskenvelope.Outcome{}, fmt.Errorf("%w: attempt %s", ErrControllerIntegrity, attempt.ID)
		}
		if rec.Outcome != nil {
			return cloneOutcome(*rec.Outcome), nil
		}
		return taskenvelope.Outcome{}, errors.New("embodied: attempt outcome remains unknown")
	}
	if c.BeforeAdmission != nil {
		c.BeforeAdmission()
	}
	if c.finalize != nil {
		if err := c.finalize(attempt); err != nil {
			return taskenvelope.Outcome{}, err
		}
	}
	rec := &ControllerRecord{AttemptID: attempt.ID, PayloadHash: payloadHash, Command: attempt.Content, Effect: taskenvelope.EffectUnknown}
	c.records[attempt.ID] = rec
	if err := c.persistLocked(); err != nil {
		delete(c.records, attempt.ID)
		return taskenvelope.Outcome{}, err
	}
	rec.EffectApplied = true
	rec.Effect = taskenvelope.EffectNone
	if c.FailPartial {
		rec.Effect = taskenvelope.EffectPartial
	}
	if err := c.persistLocked(); err != nil {
		return taskenvelope.Outcome{}, err
	}
	if c.AfterEffect != nil {
		c.AfterEffect()
	}
	outcome := taskenvelope.Outcome{Success: true, Result: "done", Effect: rec.Effect}
	if c.FailPartial {
		outcome.Success = false
		outcome.Result = "partial effect"
	}
	rec.Outcome = &outcome
	if err := c.persistLocked(); err != nil {
		rec.Outcome = nil
		return taskenvelope.Outcome{}, err
	}
	if c.LoseACK {
		return taskenvelope.Outcome{}, errors.New("embodied: result ACK lost")
	}
	return cloneOutcome(outcome), nil
}

// QueryAttempt returns an outcome only when its durable record is complete.
func (c *SimController) QueryAttempt(_ context.Context, attemptID string) (taskenvelope.Outcome, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.records[attemptID]
	if !ok || rec.Outcome == nil {
		return taskenvelope.Outcome{}, false, nil
	}
	return cloneOutcome(*rec.Outcome), true, nil
}

// AttestStop reports only terminal stops and their partial or absent effects.
func (c *SimController) AttestStop(_ context.Context, attemptID string) (taskenvelope.StopAttestation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.records[attemptID]
	if !ok {
		return taskenvelope.StopAttestation{Attested: false}, nil
	}
	if !rec.EffectApplied {
		return taskenvelope.StopAttestation{Attested: true, Effect: taskenvelope.EffectNone}, nil
	}
	if rec.Outcome == nil {
		return taskenvelope.StopAttestation{Attested: false}, nil
	}
	return taskenvelope.StopAttestation{Attested: true, Effect: rec.Effect}, nil
}

// Commands returns deep-copied controller evidence for grading.
func (c *SimController) Commands() []ControllerRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ControllerRecord, 0, len(c.records))
	for _, rec := range c.records {
		cp := *rec
		if rec.Outcome != nil {
			outcome := cloneOutcome(*rec.Outcome)
			cp.Outcome = &outcome
		}
		out = append(out, cp)
	}
	return out
}

func (c *SimController) persistLocked() error {
	wire, err := json.Marshal(c.records)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(c.path, wire, 0o600)
}

func cloneOutcome(outcome taskenvelope.Outcome) taskenvelope.Outcome {
	if outcome.Usage != nil {
		usage := *outcome.Usage
		outcome.Usage = &usage
	}
	return outcome
}
