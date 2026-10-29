package embodied

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tierrouter"
)

var (
	// ErrStaleObservation rejects expired, future or version-mismatched observations.
	ErrStaleObservation = errors.New("embodied: stale or invalid observation")
	// ErrObservationProjection rejects disclosure to an unapproved tier.
	ErrObservationProjection = errors.New("embodied: observation projection not permitted")
	// ErrObservationConflict rejects observation ID reuse with different content.
	ErrObservationConflict = errors.New("embodied: observation ID reused with different content")
	// ErrStalePlan rejects plans that are invalid under current device state.
	ErrStalePlan = errors.New("embodied: plan is not currently admissible")
	// ErrActionConflict rejects action ID reuse with different execution content.
	ErrActionConflict = errors.New("embodied: action ID reused with different content")
	// ErrControllerIntegrity reports contradictory controller evidence.
	ErrControllerIntegrity = errors.New("embodied: controller integrity event")
)

// Observation is an immutable, versioned perception and disclosure envelope.
type Observation struct {
	RootTaskID         string            `json:"root_task_id"`
	TaskEpoch          int64             `json:"task_epoch"`
	ID                 string            `json:"id"`
	Sequence           uint64            `json:"sequence"`
	CapturedAt         time.Time         `json:"captured_at"`
	ClockDomain        string            `json:"clock_domain"`
	MaxAge             time.Duration     `json:"max_age"`
	Frame              string            `json:"frame"`
	Units              string            `json:"units"`
	CalibrationVersion string            `json:"calibration_version"`
	MapVersion         string            `json:"map_version"`
	SiteStateVersion   uint64            `json:"site_state_version"`
	Summary            string            `json:"summary,omitempty"`
	MediaRef           string            `json:"media_ref,omitempty"`
	Content            json.RawMessage   `json:"content"`
	SummaryTiers       []tierrouter.Tier `json:"summary_tiers,omitempty"`
	MediaTiers         []tierrouter.Tier `json:"media_tiers,omitempty"`
}

// ProjectedObservation is the permitted summary or opaque media reference for one tier.
type ProjectedObservation struct {
	RootTaskID       string `json:"root_task_id"`
	TaskEpoch        int64  `json:"task_epoch"`
	ID               string `json:"id"`
	Sequence         uint64 `json:"sequence"`
	SiteStateVersion uint64 `json:"site_state_version"`
	Summary          string `json:"summary,omitempty"`
	MediaRef         string `json:"media_ref,omitempty"`
}

// WorldState is the trusted host's current state used for observation and action admission.
type WorldState struct {
	RootTaskID         string
	TaskEpoch          int64
	Frame              string
	Units              string
	CalibrationVersion string
	MapVersion         string
	SiteStateVersion   uint64
	Conditions         map[string]bool
}

// ValidateAt checks observation identity, freshness and current world versions.
func (o *Observation) ValidateAt(now time.Time, current *WorldState) error {
	if o.RootTaskID == "" || o.ID == "" || o.Sequence == 0 || o.ClockDomain != "host" || o.MaxAge <= 0 ||
		o.Frame == "" || o.Units == "" || o.CalibrationVersion == "" || o.MapVersion == "" || o.SiteStateVersion == 0 ||
		o.CapturedAt.IsZero() || now.Before(o.CapturedAt) || now.Sub(o.CapturedAt) > o.MaxAge ||
		o.RootTaskID != current.RootTaskID || o.TaskEpoch != current.TaskEpoch || o.Frame != current.Frame ||
		o.Units != current.Units || o.CalibrationVersion != current.CalibrationVersion || o.MapVersion != current.MapVersion ||
		o.SiteStateVersion != current.SiteStateVersion {
		return ErrStaleObservation
	}
	for _, tier := range append(append([]tierrouter.Tier(nil), o.SummaryTiers...), o.MediaTiers...) {
		if !recognizedTier(tier) {
			return ErrStaleObservation
		}
	}
	var content map[string]any
	if len(o.Content) == 0 || json.Unmarshal(o.Content, &content) != nil {
		return ErrStaleObservation
	}
	return nil
}

// Project returns only the summary or opaque media reference permitted for tier.
func (o *Observation) Project(tier tierrouter.Tier) (*ProjectedObservation, error) {
	if !recognizedTier(tier) {
		return nil, fmt.Errorf("%w: tier %s", ErrObservationProjection, tier)
	}
	projected := &ProjectedObservation{RootTaskID: o.RootTaskID, TaskEpoch: o.TaskEpoch, ID: o.ID, Sequence: o.Sequence, SiteStateVersion: o.SiteStateVersion}
	if tierAllowed(o.SummaryTiers, tier) {
		projected.Summary = o.Summary
	}
	if tierAllowed(o.MediaTiers, tier) {
		projected.MediaRef = o.MediaRef
	}
	if projected.Summary == "" && projected.MediaRef == "" {
		return nil, fmt.Errorf("%w: tier %s", ErrObservationProjection, tier)
	}
	return projected, nil
}

func tierAllowed(tiers []tierrouter.Tier, want tierrouter.Tier) bool {
	for _, tier := range tiers {
		if tier == want {
			return true
		}
	}
	return false
}

func recognizedTier(tier tierrouter.Tier) bool {
	return tier == tierrouter.TierDevice || tier == tierrouter.TierEdge || tier == tierrouter.TierCloud
}

func observationHash(o *Observation) string {
	return canonicalHash(o)
}

func cloneObservation(obs *Observation) *Observation {
	out := *obs
	out.Content = append(json.RawMessage(nil), obs.Content...)
	out.SummaryTiers = append([]tierrouter.Tier(nil), obs.SummaryTiers...)
	out.MediaTiers = append([]tierrouter.Tier(nil), obs.MediaTiers...)
	return &out
}

func canonicalHash(value any) string {
	wire, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(wire)
	return hex.EncodeToString(sum[:])
}
