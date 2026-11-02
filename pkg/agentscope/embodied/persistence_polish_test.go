package embodied

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/taskenvelope"
)

func TestRegisterRootMetadataFailureCanRetry(t *testing.T) {
	g, _, _ := newTestGateway(t)
	goodPath := g.config.MetadataPath
	blocker := goodPath + ".block"
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.config.MetadataPath = blocker + "/metadata.json"
	if err := g.RegisterRoot("root-1", 10); err == nil {
		t.Fatal("metadata failure ignored")
	}
	g.config.MetadataPath = goodPath
	if err := g.RegisterRoot("root-1", 10); err != nil {
		t.Fatalf("idempotent root metadata retry: %v", err)
	}
}

func TestRegisterRootStoreFailureCanRetry(t *testing.T) {
	g, _, _ := newTestGateway(t)
	goodPath := g.config.LedgerPath
	dir := filepath.Dir(goodPath)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.RegisterRoot("root-2", 1); err == nil {
		t.Fatal("store registration failure ignored")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := g.RegisterRoot("root-2", 1); err != nil {
		t.Fatalf("retry after restoring store persistence: %v", err)
	}
	reopened, err := taskenvelope.Open(goodPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Root("root-2"); !ok {
		t.Fatal("retried root missing from durable store")
	}
}

func TestControllerFirstPersistFailureRollsBack(t *testing.T) {
	path := t.TempDir() + "/controller.json"
	controller, err := OpenSimController(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocker := path + ".block"
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	controller.path = blocker + "/controller.json"
	attempt := taskenvelope.Attempt{ID: "first-persist", Content: `{"action":"pick"}`}
	if _, err := controller.Do(context.Background(), attempt); err == nil {
		t.Fatal("first persist failure ignored")
	}
	if _, ok := controller.records[attempt.ID]; ok {
		t.Fatal("failed first record remained in memory")
	}
	controller.path = path
	if _, err := controller.Do(context.Background(), attempt); err != nil {
		t.Fatalf("retry after restoring persistence: %v", err)
	}
	if len(controller.Commands()) != 1 {
		t.Fatalf("commands: %d", len(controller.Commands()))
	}
}

func TestRecoverCancellationUpdatesMetadata(t *testing.T) {
	g, _, now := newTestGateway(t)
	action := testAction(now)
	if _, err := g.Execute(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	world := testWorld()
	world.TaskEpoch = 2
	if err := g.SetWorldState(world); err != nil {
		t.Fatal(err)
	}
	if err := g.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !g.canceledRoots["root-1"] {
		t.Fatal("recovery did not persist canceled root")
	}
}

func TestRegisterRootConcurrentStoreAccess(t *testing.T) {
	g, _, now := newTestGateway(t)
	action := testAction(now)
	if _, err := g.Execute(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_ = g.RegisterRoot("root-1", 10)
		}()
		go func() {
			defer wg.Done()
			_, _ = g.ActionRecord(action.ID)
		}()
		go func() {
			defer wg.Done()
			_ = g.CancelRoot("root-1")
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
		t.Fatal("concurrent root registration and store access deadlocked")
	}
}
