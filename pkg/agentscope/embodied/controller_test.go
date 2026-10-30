package embodied

import (
	"context"
	"errors"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/taskenvelope"
)

func TestControllerDuplicateAndConflict(t *testing.T) {
	controller, err := OpenSimController(t.TempDir()+"/controller.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	attempt := taskenvelope.Attempt{ID: "attempt-1", RecordID: "action-1", Content: `{"action":"pick"}`}
	if _, err := controller.Do(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Do(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if len(controller.Commands()) != 1 {
		t.Fatalf("duplicate effect: %d", len(controller.Commands()))
	}
	attempt.Content = `{"action":"deliver"}`
	if _, err := controller.Do(context.Background(), attempt); !errors.Is(err, ErrControllerIntegrity) {
		t.Fatalf("payload conflict: %v", err)
	}
	if len(controller.Commands()) != 1 {
		t.Fatal("conflict changed controller evidence")
	}
}

func TestControllerCrashAfterEffectReconcilesUnknown(t *testing.T) {
	path := t.TempDir() + "/controller.json"
	controller, err := OpenSimController(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	controller.AfterEffect = func() { panic("simulated crash") }
	attempt := taskenvelope.Attempt{ID: "attempt-crash", Content: `{"action":"pick"}`}
	func() {
		defer func() { _ = recover() }()
		_, _ = controller.Do(context.Background(), attempt)
	}()
	reopened, err := OpenSimController(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := reopened.QueryAttempt(context.Background(), attempt.ID); ok {
		t.Fatal("effect without durable outcome reported as accounted")
	}
	if _, err := reopened.Do(context.Background(), attempt); err == nil {
		t.Fatal("uncertain attempt blindly redispatched")
	}
	if len(reopened.Commands()) != 1 {
		t.Fatalf("commands: %d", len(reopened.Commands()))
	}
}
