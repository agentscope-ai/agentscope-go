package app

import (
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/messagebus"
)

// --- Registry operations on InMemoryMessageBus ---

func TestRegistrySetAndGet(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	if err := bus.RegistrySet("k1", "f1", []byte("v1")); err != nil {
		t.Fatalf("RegistrySet: %v", err)
	}

	got, err := bus.RegistryGet("k1", "f1")
	if err != nil {
		t.Fatalf("RegistryGet: %v", err)
	}
	if string(got) != "v1" {
		t.Errorf("got %q, want %q", got, "v1")
	}
}

func TestRegistryGetMissing(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	got, err := bus.RegistryGet("nokey", "nofield")
	if err != nil {
		t.Fatalf("RegistryGet: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing key, got %q", got)
	}
}

func TestRegistryGetAll(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	_ = bus.RegistrySet("k", "a", []byte("1"))
	_ = bus.RegistrySet("k", "b", []byte("2"))

	all, err := bus.RegistryGetAll("k")
	if err != nil {
		t.Fatalf("RegistryGetAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d fields, want 2", len(all))
	}
	if string(all["a"]) != "1" || string(all["b"]) != "2" {
		t.Errorf("unexpected values: %v", all)
	}
}

func TestRegistryGetAllEmpty(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	all, err := bus.RegistryGetAll("empty")
	if err != nil {
		t.Fatalf("RegistryGetAll: %v", err)
	}
	if all != nil {
		t.Errorf("expected nil for empty key, got %v", all)
	}
}

func TestRegistryDel(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	_ = bus.RegistrySet("k", "a", []byte("1"))
	_ = bus.RegistrySet("k", "b", []byte("2"))

	if err := bus.RegistryDel("k", "a"); err != nil {
		t.Fatalf("RegistryDel: %v", err)
	}

	got, _ := bus.RegistryGet("k", "a")
	if got != nil {
		t.Errorf("expected nil after delete, got %q", got)
	}

	got, _ = bus.RegistryGet("k", "b")
	if string(got) != "2" {
		t.Errorf("unrelated field should remain, got %q", got)
	}
}

func TestRegistryDrop(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	_ = bus.RegistrySet("k", "a", []byte("1"))
	_ = bus.RegistrySet("k", "b", []byte("2"))

	if err := bus.RegistryDrop("k"); err != nil {
		t.Fatalf("RegistryDrop: %v", err)
	}

	all, _ := bus.RegistryGetAll("k")
	if all != nil {
		t.Errorf("expected nil after drop, got %v", all)
	}
}

func TestRegistryPayloadIsolation(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	original := []byte("original")
	_ = bus.RegistrySet("k", "f", original)

	original[0] = 'X'

	got, _ := bus.RegistryGet("k", "f")
	if string(got) != "original" {
		t.Errorf("payload mutated: got %q, want %q", got, "original")
	}
}

// --- SessionProjection SetEntry / ListEntries / DeleteEntry / PurgeEntries ---

func TestSessionProjectionEntries(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	proj := NewSessionProjection(bus)

	payload := map[string]any{"foo": "bar", "count": float64(42)}
	if err := proj.SetEntry("sid-1", "mykind", "entry-1", payload); err != nil {
		t.Fatalf("SetEntry: %v", err)
	}

	entries, err := proj.ListEntries("sid-1", "mykind")
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries["entry-1"]
	if e["foo"] != "bar" {
		t.Errorf("payload foo = %v, want %q", e["foo"], "bar")
	}
}

func TestSessionProjectionDeleteEntry(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	proj := NewSessionProjection(bus)
	_ = proj.SetEntry("sid", "k", "e1", map[string]any{"x": "1"})
	_ = proj.SetEntry("sid", "k", "e2", map[string]any{"x": "2"})

	if err := proj.DeleteEntry("sid", "k", "e1"); err != nil {
		t.Fatalf("DeleteEntry: %v", err)
	}

	entries, _ := proj.ListEntries("sid", "k")
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if _, ok := entries["e2"]; !ok {
		t.Error("e2 should remain")
	}
}

func TestSessionProjectionPurgeEntries(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	proj := NewSessionProjection(bus)
	_ = proj.SetEntry("sid", "k", "e1", map[string]any{"x": "1"})
	_ = proj.SetEntry("sid", "k", "e2", map[string]any{"x": "2"})

	if err := proj.PurgeEntries("sid", "k"); err != nil {
		t.Fatalf("PurgeEntries: %v", err)
	}

	entries, _ := proj.ListEntries("sid", "k")
	if entries != nil {
		t.Errorf("expected nil after purge, got %v", entries)
	}
}

func TestSessionProjectionFallbackNoBus(t *testing.T) {
	proj := NewSessionProjection(nil) // nil bus triggers fallback

	payload := map[string]any{"key": "value"}
	if err := proj.SetEntry("s1", "kind", "e1", payload); err != nil {
		t.Fatalf("SetEntry: %v", err)
	}

	entries, err := proj.ListEntries("s1", "kind")
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries["e1"]["key"] != "value" {
		t.Errorf("unexpected payload: %v", entries["e1"])
	}

	_ = proj.DeleteEntry("s1", "kind", "e1")
	entries, _ = proj.ListEntries("s1", "kind")
	if entries != nil {
		t.Errorf("expected nil after delete, got %v", entries)
	}
}

// --- SubagentHitlProjector: MaybeProject ---

func TestMaybeProjectRequireUserConfirm(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	proj := NewSessionProjection(bus)
	hitl := NewSubagentHitlProjector(proj)

	hitl.Register("worker-1", "leader-1")

	evt := event.NewRequireUserConfirmEvent("reply-abc", []message.ToolCallBlock{
		{Name: "bash", ID: "tc1"},
	})

	if err := hitl.MaybeProject(evt, "worker-1", "my-agent"); err != nil {
		t.Fatalf("MaybeProject: %v", err)
	}

	// Verify the entry was stored.
	entries, err := proj.ListEntries("leader-1", subagentHitlKind)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}

	entryID := "worker-1:reply-abc"
	entry, ok := entries[entryID]
	if !ok {
		t.Fatalf("entry %q not found", entryID)
	}
	if entry["worker_session_id"] != "worker-1" {
		t.Errorf("worker_session_id = %v", entry["worker_session_id"])
	}
	if entry["worker_agent_name"] != "my-agent" {
		t.Errorf("worker_agent_name = %v", entry["worker_agent_name"])
	}
	if entry["reply_id"] != "reply-abc" {
		t.Errorf("reply_id = %v", entry["reply_id"])
	}
	if entry["event_type"] != string(event.EventRequireUserConfirm) {
		t.Errorf("event_type = %v", entry["event_type"])
	}
}

func TestMaybeProjectReplyEndRemovesEntry(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	proj := NewSessionProjection(bus)
	hitl := NewSubagentHitlProjector(proj)

	hitl.Register("worker-1", "leader-1")

	// First, project a request.
	reqEvt := event.NewRequireUserConfirmEvent("reply-xyz", nil)
	_ = hitl.MaybeProject(reqEvt, "worker-1", "agent")

	// Then simulate a reply end.
	endEvt := event.NewReplyEndEvent("session-1", "reply-xyz")
	if err := hitl.MaybeProject(endEvt, "worker-1", "agent"); err != nil {
		t.Fatalf("MaybeProject (end): %v", err)
	}

	entries, _ := proj.ListEntries("leader-1", subagentHitlKind)
	if entries != nil {
		t.Errorf("expected nil entries after reply end, got %v", entries)
	}
}

func TestMaybeProjectIgnoresNonHitlEvents(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	proj := NewSessionProjection(bus)
	hitl := NewSubagentHitlProjector(proj)

	hitl.Register("worker-1", "leader-1")

	// A text block event should be ignored.
	evt := event.NewTextBlockStartEvent("reply-1", "block-1")
	if err := hitl.MaybeProject(evt, "worker-1", "agent"); err != nil {
		t.Fatalf("MaybeProject: %v", err)
	}

	entries, _ := proj.ListEntries("leader-1", subagentHitlKind)
	if entries != nil {
		t.Errorf("expected nil entries for non-HITL event, got %v", entries)
	}
}

func TestMaybeProjectStandaloneWorkerNoOp(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	proj := NewSessionProjection(bus)
	hitl := NewSubagentHitlProjector(proj)

	// Don't register the worker.
	evt := event.NewRequireUserConfirmEvent("reply-1", nil)
	if err := hitl.MaybeProject(evt, "unregistered-worker", "agent"); err != nil {
		t.Fatalf("MaybeProject: %v", err)
	}

	// No leader to project onto.
	entries, _ := proj.ListEntries("any-leader", subagentHitlKind)
	if entries != nil {
		t.Errorf("expected nil entries for standalone worker, got %v", entries)
	}
}

// --- SubagentHitlProjector: Resolve ---

func TestResolveRouting(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	proj := NewSessionProjection(bus)
	hitl := NewSubagentHitlProjector(proj)

	hitl.Register("worker-A", "leader-1")
	hitl.Register("worker-B", "leader-1")

	// Project requests from two different workers.
	evtA := event.NewRequireUserConfirmEvent("reply-A", nil)
	_ = hitl.MaybeProject(evtA, "worker-A", "agentA")

	evtB := event.NewRequireExternalExecutionEvent("reply-B", nil)
	_ = hitl.MaybeProject(evtB, "worker-B", "agentB")

	// Resolve reply-A should find worker-A.
	wsid, found := hitl.Resolve("leader-1", "reply-A")
	if !found {
		t.Fatal("Resolve: not found for reply-A")
	}
	if wsid != "worker-A" {
		t.Errorf("Resolve reply-A: got %q, want %q", wsid, "worker-A")
	}

	// Resolve reply-B should find worker-B.
	wsid, found = hitl.Resolve("leader-1", "reply-B")
	if !found {
		t.Fatal("Resolve: not found for reply-B")
	}
	if wsid != "worker-B" {
		t.Errorf("Resolve reply-B: got %q, want %q", wsid, "worker-B")
	}

	// Resolve unknown should not find.
	_, found = hitl.Resolve("leader-1", "reply-unknown")
	if found {
		t.Error("Resolve: should not find unknown reply")
	}
}

// --- SubagentHitlProjector: DropWorker ---

func TestDropWorkerCleanup(t *testing.T) {
	bus := messagebus.NewInMemoryMessageBus()
	defer bus.Close()

	proj := NewSessionProjection(bus)
	hitl := NewSubagentHitlProjector(proj)

	hitl.Register("worker-A", "leader-1")
	hitl.Register("worker-B", "leader-1")

	// Project from both workers.
	evtA1 := event.NewRequireUserConfirmEvent("r1", nil)
	_ = hitl.MaybeProject(evtA1, "worker-A", "agentA")

	evtA2 := event.NewRequireExternalExecutionEvent("r2", nil)
	_ = hitl.MaybeProject(evtA2, "worker-A", "agentA")

	evtB := event.NewRequireUserConfirmEvent("r3", nil)
	_ = hitl.MaybeProject(evtB, "worker-B", "agentB")

	// Drop worker-A.
	if err := hitl.DropWorker("leader-1", "worker-A"); err != nil {
		t.Fatalf("DropWorker: %v", err)
	}

	// worker-A entries should be gone.
	_, found := hitl.Resolve("leader-1", "r1")
	if found {
		t.Error("r1 should be removed after DropWorker")
	}
	_, found = hitl.Resolve("leader-1", "r2")
	if found {
		t.Error("r2 should be removed after DropWorker")
	}

	// worker-B entries should remain.
	wsid, found := hitl.Resolve("leader-1", "r3")
	if !found {
		t.Error("r3 should still exist")
	}
	if wsid != "worker-B" {
		t.Errorf("r3 worker = %q, want %q", wsid, "worker-B")
	}
}
