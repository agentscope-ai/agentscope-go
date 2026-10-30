package embodied

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/taskenvelope"
)

func TestGatewayActionIdentityAndCurrentState(t *testing.T) {
	g, controller, now := newTestGateway(t)
	action := testAction(now)
	if _, err := g.Execute(context.Background(), action); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(controller.Commands()) != 1 {
		t.Fatalf("commands: %d", len(controller.Commands()))
	}
	changed := cloneAction(action)
	changed.Plan.Steps[0].Destination = "B"
	if _, err := g.Execute(context.Background(), changed); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("changed destination: %v", err)
	}
	changed = cloneAction(action)
	changed.Plan.Steps[0].Arguments = []byte(`{"speed":2}`)
	if _, err := g.Execute(context.Background(), changed); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("changed arguments: %v", err)
	}
	world := testWorld()
	world.TaskEpoch = 2
	_ = g.SetWorldState(world)
	if _, err := g.Execute(context.Background(), action); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("stale replay: %v", err)
	}
	rec, ok := g.ActionRecord(action.ID)
	if !ok || rec.State != taskenvelope.StateCompleted || len(controller.Commands()) != 1 {
		t.Fatalf("historical record=%+v commands=%d", rec, len(controller.Commands()))
	}
}

func TestGatewayRejectsObservationConflict(t *testing.T) {
	g, _, now := newTestGateway(t)
	obs := testObservation(now)
	obs.ID = "obs-1"
	obs.Summary = "changed"
	if err := g.RegisterObservation(&obs); !errors.Is(err, ErrObservationConflict) {
		t.Fatalf("observation conflict: %v", err)
	}
}

func TestGatewayLostACKRecoveryDoesNotRepeatEffect(t *testing.T) {
	g, controller, now := newTestGateway(t)
	controller.LoseACK = true
	action := testAction(now)
	rec, err := g.Execute(context.Background(), action)
	if err == nil || rec.State != taskenvelope.StateUnknownEffect {
		t.Fatalf("record=%+v err=%v", rec, err)
	}
	if len(controller.Commands()) != 1 {
		t.Fatalf("commands after lost ACK: %d", len(controller.Commands()))
	}
	reopened, reopenedController := reopenGateway(t, g)
	if err := reopened.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	rec, err = reopened.Execute(context.Background(), action)
	if err != nil || rec.State != taskenvelope.StateCompleted {
		t.Fatalf("recovered record=%+v err=%v", rec, err)
	}
	if len(reopenedController.Commands()) != 1 {
		t.Fatalf("duplicate effect: %d", len(reopenedController.Commands()))
	}
}

func TestControllerFinalAdmissionRevocation(t *testing.T) {
	g, controller, now := newTestGateway(t)
	controller.BeforeAdmission = func() {
		world := testWorld()
		world.Conditions["clear"] = false
		_ = g.SetWorldState(world)
	}
	rec, err := g.Execute(context.Background(), testAction(now))
	if err == nil || rec.State != taskenvelope.StateUnknownEffect {
		t.Fatalf("record=%+v err=%v", rec, err)
	}
	if len(controller.Commands()) != 0 {
		t.Fatal("effect applied after destination/precondition revocation")
	}
}

func TestGatewayPartialEffectAndCancellation(t *testing.T) {
	g, controller, now := newTestGateway(t)
	controller.FailPartial = true
	rec, err := g.Execute(context.Background(), testAction(now))
	if err != nil || rec.State != taskenvelope.StateFailed || rec.Effect != taskenvelope.EffectPartial {
		t.Fatalf("partial record=%+v err=%v", rec, err)
	}
	if err := g.CancelRoot("root-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Execute(context.Background(), testAction(now)); !errors.Is(err, ErrStalePlan) {
		t.Fatalf("canceled root replay: %v", err)
	}
	if len(controller.Commands()) != 1 {
		t.Fatalf("commands: %d", len(controller.Commands()))
	}
}

func newTestGateway(t *testing.T) (*Gateway, *SimController, time.Time) {
	t.Helper()
	now := time.Now()
	dir := t.TempDir()
	controller, err := OpenSimController(dir+"/controller.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGateway(&GatewayConfig{
		LedgerPath: dir + "/ledger.json", MetadataPath: dir + "/metadata.json",
		Controller: controller, Skills: []Skill{{Name: "pick", Version: "v1", Destinations: []string{"A"}, Preconditions: []string{"clear"}}},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.RegisterRoot("root-1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := g.AdvanceEpoch("root-1"); err != nil {
		t.Fatal(err)
	}
	_ = g.SetWorldState(testWorld())
	obs := testObservation(now)
	if err := g.RegisterObservation(&obs); err != nil {
		t.Fatal(err)
	}
	return g, controller, now
}

func reopenGateway(t *testing.T, old *Gateway) (*Gateway, *SimController) {
	t.Helper()
	controller, err := OpenSimController(old.config.Controller.(*SimController).path, nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGateway(&GatewayConfig{
		LedgerPath: old.config.LedgerPath, MetadataPath: old.config.MetadataPath,
		Controller: controller, Skills: old.config.Skills, Now: old.config.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = g.SetWorldState(testWorld())
	return g, controller
}

func testAction(now time.Time) *Action {
	return &Action{
		ID: "action-1", Observation: testObservation(now),
		Plan: Plan{
			RootTaskID: "root-1", TaskEpoch: 1, ID: "plan-1", ObservationID: "obs-1", ObservationSequence: 1,
			SiteStateVersion: 1, ValidUntil: now.Add(time.Minute),
			Steps: []PlanStep{{ID: "pick-blue", Skill: "pick", SkillVersion: "v1", Destination: "A", Arguments: []byte(`{"speed":1}`), Preconditions: []string{"clear"}}},
		},
	}
}
