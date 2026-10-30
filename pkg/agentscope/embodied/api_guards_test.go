package embodied

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/taskenvelope"
)

func TestPublicAPINilAndScopeGuards(t *testing.T) {
	if _, err := NewGateway(nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil config: %v", err)
	}
	g, _, now := newTestGateway(t)
	if _, err := g.Execute(context.Background(), nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil action: %v", err)
	}
	if err := g.RegisterObservation(nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil observation: %v", err)
	}
	if err := g.SetWorldState(nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil world: %v", err)
	}
	obs := testObservation(now)
	if err := obs.ValidateAt(now, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil current world: %v", err)
	}
	var nilObs *Observation
	if err := nilObs.ValidateAt(now, testWorld()); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil observation receiver: %v", err)
	}
	var nilAction *Action
	if _, err := nilAction.Scope(); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil action scope: %v", err)
	}
	action := testAction(now)
	action.StepIndex = len(action.Plan.Steps)
	if _, err := action.Scope(); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("out-of-range scope: %v", err)
	}
}

func TestGatewayRejectsExistingFinalizeHook(t *testing.T) {
	dir := t.TempDir()
	controller, err := OpenSimController(dir+"/controller.json", func(taskenvelope.Attempt) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewGateway(&GatewayConfig{
		LedgerPath: dir + "/ledger.json", MetadataPath: dir + "/metadata.json",
		Controller: controller, Skills: []Skill{{Name: "pick", Version: "v1", Destinations: []string{"A"}}},
		Now: time.Now,
	})
	if err == nil {
		t.Fatal("existing finalize hook overwritten")
	}
}
