package embodied

import (
	"encoding/json"
	"fmt"
	"time"
)

// Skill registers an immutable skill version, destinations and required preconditions.
type Skill struct {
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	Destinations  []string `json:"destinations"`
	Preconditions []string `json:"preconditions"`
}

// PlanStep is one host-scheduled skill invocation with immutable arguments and preconditions.
type PlanStep struct {
	ID            string          `json:"id"`
	Skill         string          `json:"skill"`
	SkillVersion  string          `json:"skill_version"`
	Destination   string          `json:"destination"`
	Arguments     json.RawMessage `json:"arguments"`
	Preconditions []string        `json:"preconditions"`
}

// Plan is host-scheduled. Gateway executes one action and never advances steps automatically.
type Plan struct {
	RootTaskID          string     `json:"root_task_id"`
	TaskEpoch           int64      `json:"task_epoch"`
	ID                  string     `json:"id"`
	ObservationID       string     `json:"observation_id"`
	ObservationSequence uint64     `json:"observation_sequence"`
	SiteStateVersion    uint64     `json:"site_state_version"`
	ValidUntil          time.Time  `json:"valid_until"`
	Steps               []PlanStep `json:"steps"`
}

// Action binds one selected plan step to its exact observation and host-allocated ID.
type Action struct {
	ID          string      `json:"id"`
	Plan        Plan        `json:"plan"`
	StepIndex   int         `json:"step_index"`
	Observation Observation `json:"observation"`
}

// Scope returns the skill, destination and version authority tuple.
func (a *Action) Scope() string {
	step := a.Plan.Steps[a.StepIndex]
	return fmt.Sprintf("%s@%s@%s", step.Skill, step.Destination, step.SkillVersion)
}

// ValidateBasic checks the immutable plan, step, observation and deadline fields.
func (a *Action) ValidateBasic(now time.Time) error {
	if a.ID == "" || a.Plan.RootTaskID == "" || a.Plan.TaskEpoch != a.Observation.TaskEpoch ||
		a.Plan.RootTaskID != a.Observation.RootTaskID || a.Plan.ID == "" || a.Plan.ObservationID != a.Observation.ID ||
		a.Plan.ObservationSequence != a.Observation.Sequence || a.Plan.SiteStateVersion != a.Observation.SiteStateVersion ||
		a.Plan.ValidUntil.IsZero() || !now.Before(a.Plan.ValidUntil) || a.StepIndex < 0 || a.StepIndex >= len(a.Plan.Steps) {
		return ErrStalePlan
	}
	step := a.Plan.Steps[a.StepIndex]
	if step.ID == "" || step.Skill == "" || step.SkillVersion == "" || step.Destination == "" {
		return ErrStalePlan
	}
	var arguments map[string]any
	if len(step.Arguments) == 0 || json.Unmarshal(step.Arguments, &arguments) != nil {
		return ErrStalePlan
	}
	return nil
}

func (a *Action) canonicalContent() (string, error) {
	wire, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	return string(wire), nil
}

func cloneAction(action *Action) *Action {
	out := *action
	out.Plan.Steps = append([]PlanStep(nil), action.Plan.Steps...)
	for i := range out.Plan.Steps {
		out.Plan.Steps[i].Arguments = append(json.RawMessage(nil), out.Plan.Steps[i].Arguments...)
		out.Plan.Steps[i].Preconditions = append([]string(nil), out.Plan.Steps[i].Preconditions...)
	}
	out.Observation = *cloneObservation(&action.Observation)
	return &out
}
