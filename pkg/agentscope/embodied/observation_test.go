package embodied

import (
	"errors"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tierrouter"
)

func TestObservationValidationAndProjection(t *testing.T) {
	now := time.Now()
	obs := testObservation(now)
	world := testWorld()
	if err := obs.ValidateAt(now, world); err != nil {
		t.Fatalf("valid observation: %v", err)
	}
	projected, err := obs.Project(tierrouter.TierEdge)
	if err != nil {
		t.Fatal(err)
	}
	if projected.Summary != "blue tray" || projected.MediaRef != "" {
		t.Fatalf("projection: %+v", projected)
	}
	if _, err := obs.Project(tierrouter.TierCloud); err == nil {
		t.Fatal("unapproved projection allowed")
	}
	cloudObs := obs
	cloudObs.MediaTiers = []tierrouter.Tier{tierrouter.TierCloud}
	cloudProjected, err := cloudObs.Project(tierrouter.TierCloud)
	if err != nil || cloudProjected.MediaRef != "opaque://frame/1" || cloudProjected.Summary != "" {
		t.Fatalf("media projection: %+v err=%v", cloudProjected, err)
	}
}

func TestObservationRejectsInvalidState(t *testing.T) {
	now := time.Now()
	tests := map[string]func(*Observation, *WorldState){
		"expired":      func(o *Observation, _ *WorldState) { o.CapturedAt = now.Add(-time.Hour) },
		"future":       func(o *Observation, _ *WorldState) { o.CapturedAt = now.Add(time.Second) },
		"clock domain": func(o *Observation, _ *WorldState) { o.ClockDomain = "unknown" },
		"max age":      func(o *Observation, _ *WorldState) { o.MaxAge = 0 },
		"epoch":        func(_ *Observation, w *WorldState) { w.TaskEpoch++ },
		"frame":        func(_ *Observation, w *WorldState) { w.Frame = "other" },
		"units":        func(_ *Observation, w *WorldState) { w.Units = "cm" },
		"calibration":  func(_ *Observation, w *WorldState) { w.CalibrationVersion = "other" },
		"map":          func(_ *Observation, w *WorldState) { w.MapVersion = "other" },
		"site state":   func(_ *Observation, w *WorldState) { w.SiteStateVersion++ },
		"content":      func(o *Observation, _ *WorldState) { o.Content = []byte(`{`) },
		"sequence":     func(o *Observation, _ *WorldState) { o.Sequence = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			obs, world := testObservation(now), testWorld()
			mutate(&obs, world)
			if err := obs.ValidateAt(now, world); !errors.Is(err, ErrStaleObservation) {
				t.Fatalf("want stale observation, got %v", err)
			}
		})
	}
}

func testObservation(now time.Time) Observation {
	return Observation{
		RootTaskID: "root-1", TaskEpoch: 1, ID: "obs-1", Sequence: 1,
		CapturedAt: now.Add(-time.Second), ClockDomain: "host", MaxAge: time.Second * 5,
		Frame: "map", Units: "m", CalibrationVersion: "cal-1", MapVersion: "map-1", SiteStateVersion: 1,
		Summary: "blue tray", MediaRef: "opaque://frame/1", Content: []byte(`{"tray":"blue"}`),
		SummaryTiers: []tierrouter.Tier{tierrouter.TierDevice, tierrouter.TierEdge},
	}
}

func testWorld() *WorldState {
	return &WorldState{RootTaskID: "root-1", TaskEpoch: 1, Frame: "map", Units: "m", CalibrationVersion: "cal-1", MapVersion: "map-1", SiteStateVersion: 1, Conditions: map[string]bool{"clear": true}}
}
