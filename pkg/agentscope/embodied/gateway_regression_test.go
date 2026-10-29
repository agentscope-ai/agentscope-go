package embodied

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/taskenvelope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tierrouter"
)

func TestAdvanceEpochRejectsCompletedReplay(t *testing.T) {
	g, controller, now := newTestGateway(t)
	action := testAction(now)
	if _, err := g.Execute(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if _, err := g.AdvanceEpoch("root-1"); err != nil {
		t.Fatal(err)
	}
	rec, err := g.Execute(context.Background(), action)
	if !errors.Is(err, ErrStalePlan) || rec.State != taskenvelope.StateCompleted {
		t.Fatalf("record=%+v err=%v", rec, err)
	}
	if len(controller.Commands()) != 1 {
		t.Fatalf("commands: %d", len(controller.Commands()))
	}
}

func TestActionMetadataFailureRollsBackAndRetries(t *testing.T) {
	g, controller, now := newTestGateway(t)
	goodPath := g.config.MetadataPath
	blocker := goodPath + ".block"
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.config.MetadataPath = blocker + "/metadata.json"
	action := testAction(now)
	if _, err := g.Execute(context.Background(), action); err == nil {
		t.Fatal("metadata write failure ignored")
	}
	if _, ok := g.ActionRecord(action.ID); ok {
		t.Fatal("store submitted without durable action metadata")
	}
	if _, ok := g.actions[action.ID]; ok {
		t.Fatal("failed action metadata remained in memory")
	}
	g.config.MetadataPath = goodPath
	if _, err := g.Execute(context.Background(), action); err != nil {
		t.Fatalf("retry after restoring persistence: %v", err)
	}
	if len(controller.Commands()) != 1 {
		t.Fatalf("commands: %d", len(controller.Commands()))
	}
}

func TestObservationMetadataFailureRollsBack(t *testing.T) {
	g, _, now := newTestGateway(t)
	goodPath := g.config.MetadataPath
	blocker := goodPath + ".block"
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.config.MetadataPath = blocker + "/metadata.json"
	obs := testObservation(now)
	obs.ID = "obs-2"
	if err := g.RegisterObservation(&obs); err == nil {
		t.Fatal("observation metadata write failure ignored")
	}
	if _, ok := g.observations[obs.ID]; ok {
		t.Fatal("failed observation metadata remained in memory")
	}
	g.config.MetadataPath = goodPath
	if err := g.RegisterObservation(&obs); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryProcessesCancelingRecord(t *testing.T) {
	g, controller, now := newTestGateway(t)
	controller.Gate = make(chan struct{})
	action := testAction(now)
	done := make(chan struct{})
	go func() {
		_, _ = g.Execute(context.Background(), action)
		close(done)
	}()
	for {
		rec, _ := g.ActionRecord(action.ID)
		if rec != nil && rec.State == taskenvelope.StateInProgress {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := g.CancelRoot("root-1"); err != nil {
		t.Fatal(err)
	}
	reopened, _ := reopenGateway(t, g)
	if err := reopened.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := reopened.ActionRecord(action.ID)
	if rec.State != taskenvelope.StateUnknownEffect {
		t.Fatalf("canceling record remained unresolved: %+v", rec)
	}
	close(controller.Gate)
	<-done
}

func TestSubmissionTTLUsesHostClock(t *testing.T) {
	g, _, now := newTestGateway(t)
	action := testAction(now)
	rec, err := g.Execute(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Bounds.TTL != action.Plan.ValidUntil.Sub(now) {
		t.Fatalf("TTL=%v want %v", rec.Bounds.TTL, action.Plan.ValidUntil.Sub(now))
	}
}

func TestUnknownTierAndUnknownAttemptStopRejected(t *testing.T) {
	now := time.Now()
	obs := testObservation(now)
	obs.SummaryTiers = []tierrouter.Tier{"unknown"}
	if err := obs.ValidateAt(now, testWorld()); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("unknown tier registration: %v", err)
	}
	if _, err := obs.Project("unknown"); !errors.Is(err, ErrObservationProjection) {
		t.Fatalf("unknown tier projection: %v", err)
	}
	controller, err := OpenSimController(t.TempDir()+"/controller.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := controller.AttestStop(context.Background(), "missing")
	if err != nil || stop.Attested {
		t.Fatalf("unknown attempt stop: %+v err=%v", stop, err)
	}
}

func TestSkillRegistrationCopiesSlices(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	controller, err := OpenSimController(dir+"/controller.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &GatewayConfig{
		LedgerPath: dir + "/ledger.json", MetadataPath: dir + "/metadata.json", Controller: controller,
		Skills: []Skill{{Name: "pick", Version: "v1", Destinations: []string{"A"}, Preconditions: []string{"clear"}}},
		Now:    func() time.Time { return now },
	}
	g, err := NewGateway(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Skills[0].Preconditions[0] = "other"
	cfg.Skills[0].Destinations[0] = "B"
	if got := g.skills["pick@A@v1"].Preconditions[0]; got != "clear" {
		t.Fatalf("skill preconditions mutated: %s", got)
	}
	if got := g.skills["pick@A@v1"].Destinations[0]; got != "A" {
		t.Fatalf("skill destinations mutated: %s", got)
	}
}

func TestControllerOutcomeWriteFailureRemainsUnknown(t *testing.T) {
	path := t.TempDir() + "/controller.json"
	controller, err := OpenSimController(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocker := path + ".block"
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	controller.AfterEffect = func() { controller.path = blocker + "/outcome.json" }
	attempt := taskenvelope.Attempt{ID: "outcome-failure", Content: `{"action":"pick"}`}
	if _, err := controller.Do(context.Background(), attempt); err == nil {
		t.Fatal("outcome write failure ignored")
	}
	if _, ok, _ := controller.QueryAttempt(context.Background(), attempt.ID); ok {
		t.Fatal("unpersisted outcome became accounted")
	}
	if _, err := controller.Do(context.Background(), attempt); err == nil {
		t.Fatal("unpersisted outcome replayed as success")
	}
	controller.path = path
	reopened, err := OpenSimController(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := reopened.QueryAttempt(context.Background(), attempt.ID); ok {
		t.Fatal("disk contains a complete outcome after failed write")
	}
}

func TestConcurrentEpochAdvanceExecuteAndRecover(t *testing.T) {
	g, _, now := newTestGateway(t)
	action := testAction(now)
	if _, err := g.Execute(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _ = g.Execute(context.Background(), action)
		}()
		go func() {
			defer wg.Done()
			_, _ = g.AdvanceEpoch("root-1")
		}()
		go func() {
			defer wg.Done()
			_ = g.Recover(context.Background())
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent epoch, execute and recovery deadlocked")
	}
	if got, want := g.epochs["root-1"], g.store.Epoch("root-1"); got != want {
		t.Fatalf("gateway epoch=%d store epoch=%d", got, want)
	}
}

func TestRestartRestoresRootWithoutActions(t *testing.T) {
	g, _, now := newTestGateway(t)
	reopened, _ := reopenGateway(t, g)
	reopened.SetWorldState(testWorld())
	obs := testObservation(now)
	if err := reopened.RegisterObservation(&obs); err != nil {
		t.Fatal(err)
	}
	action := testAction(now)
	rec, err := reopened.Execute(context.Background(), action)
	if err != nil || rec.State != taskenvelope.StateCompleted {
		t.Fatalf("root without previous actions: record=%+v err=%v", rec, err)
	}
}

func TestRegisterRootValidationErrorPreserved(t *testing.T) {
	g, _, _ := newTestGateway(t)
	if err := g.RegisterRoot("root-1", -1); err == nil {
		t.Fatal("negative allowance accepted for existing root")
	}
}
