package app

import (
	"context"
	"sync"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// ---------- helpers ----------

// mockChatModel is a minimal ChatModel for creating UnifiedAgents in tests.
type mockChatModel struct{}

func (m *mockChatModel) Chat(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	return &model.ChatResponse{}, nil
}

func (m *mockChatModel) ChatStream(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, nil
}

func (m *mockChatModel) CountTokens(_ []*message.Msg, _ []model.ToolSchema) int {
	return 0
}

// testAgentFactory returns an AgentFactory that creates a simple UnifiedAgent
// with a mock model.
func testAgentFactory() AgentFactory {
	return func(session *SessionRecord) (*agent.UnifiedAgent, error) {
		return agent.NewUnifiedAgent(
			session.AgentName,
			session.SystemPrompt,
			&mockChatModel{},
		), nil
	}
}

// ---------- SessionService ----------

func TestSessionService_CreateAndGet(t *testing.T) {
	svc := NewSessionService(testAgentFactory())

	session := svc.Create(CreateSessionRequest{
		AgentName:    "test-agent",
		SystemPrompt: "You are a test assistant.",
		ModelName:    "gpt-4",
	})

	if session.ID == "" {
		t.Fatal("expected non-empty session ID")
	}
	if session.AgentName != "test-agent" {
		t.Errorf("AgentName = %q, want %q", session.AgentName, "test-agent")
	}
	if session.SystemPrompt != "You are a test assistant." {
		t.Errorf("SystemPrompt = %q, want %q", session.SystemPrompt, "You are a test assistant.")
	}
	if session.ModelName != "gpt-4" {
		t.Errorf("ModelName = %q, want %q", session.ModelName, "gpt-4")
	}

	got, ok := svc.Get(session.ID)
	if !ok {
		t.Fatal("expected to find session")
	}
	if got.ID != session.ID {
		t.Errorf("Get returned ID = %q, want %q", got.ID, session.ID)
	}
}

func TestSessionService_CreateDefaultAgentName(t *testing.T) {
	svc := NewSessionService(nil)

	session := svc.Create(CreateSessionRequest{})
	if session.AgentName != "agent" {
		t.Errorf("default AgentName = %q, want %q", session.AgentName, "agent")
	}
}

func TestSessionService_GetNotFound(t *testing.T) {
	svc := NewSessionService(nil)

	_, ok := svc.Get("nonexistent")
	if ok {
		t.Error("expected Get to return false for nonexistent session")
	}
}

func TestSessionService_List(t *testing.T) {
	svc := NewSessionService(nil)

	svc.Create(CreateSessionRequest{AgentName: "a1"})
	svc.Create(CreateSessionRequest{AgentName: "a2"})
	svc.Create(CreateSessionRequest{AgentName: "a3"})

	sessions := svc.List()
	if len(sessions) != 3 {
		t.Fatalf("List() returned %d sessions, want 3", len(sessions))
	}

	names := make(map[string]bool)
	for _, s := range sessions {
		names[s.AgentName] = true
	}
	for _, want := range []string{"a1", "a2", "a3"} {
		if !names[want] {
			t.Errorf("List() missing agent %q", want)
		}
	}
}

func TestSessionService_Delete(t *testing.T) {
	svc := NewSessionService(testAgentFactory())

	session := svc.Create(CreateSessionRequest{AgentName: "doomed"})
	id := session.ID

	// Create the agent so we also verify agent cleanup.
	_, err := svc.GetOrCreateAgent(id)
	if err != nil {
		t.Fatalf("GetOrCreateAgent: %v", err)
	}

	svc.Delete(id)

	_, ok := svc.Get(id)
	if ok {
		t.Error("expected session to be deleted")
	}

	// Agent should also be gone: GetOrCreateAgent should fail since session is deleted.
	_, err = svc.GetOrCreateAgent(id)
	if err == nil {
		t.Error("expected error after deleting session, got nil")
	}
}

func TestSessionService_GetOrCreateAgent(t *testing.T) {
	svc := NewSessionService(testAgentFactory())

	session := svc.Create(CreateSessionRequest{
		AgentName:    "my-agent",
		SystemPrompt: "hello",
	})

	ag, err := svc.GetOrCreateAgent(session.ID)
	if err != nil {
		t.Fatalf("GetOrCreateAgent: %v", err)
	}
	if ag.Name() != "my-agent" {
		t.Errorf("agent name = %q, want %q", ag.Name(), "my-agent")
	}

	// Second call should return the same agent (cached).
	ag2, err := svc.GetOrCreateAgent(session.ID)
	if err != nil {
		t.Fatalf("GetOrCreateAgent (second call): %v", err)
	}
	if ag != ag2 {
		t.Error("expected same agent instance on second call")
	}
}

func TestSessionService_GetOrCreateAgent_NoFactory(t *testing.T) {
	svc := NewSessionService(nil)

	session := svc.Create(CreateSessionRequest{AgentName: "test"})
	_, err := svc.GetOrCreateAgent(session.ID)
	if err == nil {
		t.Error("expected error when no factory is configured")
	}
}

func TestSessionService_GetOrCreateAgent_NotFound(t *testing.T) {
	svc := NewSessionService(testAgentFactory())

	_, err := svc.GetOrCreateAgent("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestSessionService_GetOrCreateAgent_RegistersAsMember(t *testing.T) {
	svc := NewSessionService(testAgentFactory())

	session := svc.Create(CreateSessionRequest{AgentName: "primary"})
	_, err := svc.GetOrCreateAgent(session.ID)
	if err != nil {
		t.Fatalf("GetOrCreateAgent: %v", err)
	}

	members, err := svc.ListMembers(session.ID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(members))
	}
	if members[0].Name != "primary" {
		t.Errorf("member name = %q, want %q", members[0].Name, "primary")
	}
	if members[0].Type != "unified" {
		t.Errorf("member type = %q, want %q", members[0].Type, "unified")
	}
}

func TestSessionService_AddMember(t *testing.T) {
	svc := NewSessionService(testAgentFactory())

	session := svc.Create(CreateSessionRequest{AgentName: "main"})

	extra := agent.NewUnifiedAgent("helper", "assist", &mockChatModel{})
	err := svc.AddMember(session.ID, extra, "helper")
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	members, err := svc.ListMembers(session.ID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(members))
	}
	if members[0].Name != "helper" {
		t.Errorf("member name = %q, want %q", members[0].Name, "helper")
	}
}

func TestSessionService_AddMember_SessionNotFound(t *testing.T) {
	svc := NewSessionService(nil)

	ag := agent.NewUnifiedAgent("x", "x", &mockChatModel{})
	err := svc.AddMember("bogus", ag, "worker")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestSessionService_ListMembers_SessionNotFound(t *testing.T) {
	svc := NewSessionService(nil)

	_, err := svc.ListMembers("bogus")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestSessionRecord_ToResponse(t *testing.T) {
	svc := NewSessionService(testAgentFactory())

	session := svc.Create(CreateSessionRequest{
		AgentName:    "bot",
		SystemPrompt: "prompt",
		ModelName:    "claude",
	})

	// Add a member so we can check the response.
	ag := agent.NewUnifiedAgent("m1", "", &mockChatModel{})
	_ = svc.AddMember(session.ID, ag, "helper")

	resp := session.ToResponse()
	if resp.ID != session.ID {
		t.Errorf("resp.ID = %q, want %q", resp.ID, session.ID)
	}
	if resp.AgentName != "bot" {
		t.Errorf("resp.AgentName = %q, want %q", resp.AgentName, "bot")
	}
	if resp.SystemPrompt != "prompt" {
		t.Errorf("resp.SystemPrompt = %q, want %q", resp.SystemPrompt, "prompt")
	}
	if resp.ModelName != "claude" {
		t.Errorf("resp.ModelName = %q, want %q", resp.ModelName, "claude")
	}
	if len(resp.Members) != 1 || resp.Members[0] != "m1" {
		t.Errorf("resp.Members = %v, want [m1]", resp.Members)
	}
}

// ---------- ChatRunRegistry ----------

func TestChatRunRegistry_TryAcquireAndRelease(t *testing.T) {
	r := NewChatRunRegistry()

	if !r.TryAcquire("s1") {
		t.Error("expected TryAcquire to succeed")
	}
	if r.TryAcquire("s1") {
		t.Error("expected TryAcquire to fail for already-acquired session")
	}
	if !r.IsRunning("s1") {
		t.Error("expected IsRunning = true")
	}

	r.Release("s1")
	if r.IsRunning("s1") {
		t.Error("expected IsRunning = false after Release")
	}

	// Should be able to re-acquire after release.
	if !r.TryAcquire("s1") {
		t.Error("expected TryAcquire to succeed after Release")
	}
}

func TestChatRunRegistry_IndependentSessions(t *testing.T) {
	r := NewChatRunRegistry()

	if !r.TryAcquire("a") {
		t.Fatal("expected TryAcquire for 'a' to succeed")
	}
	if !r.TryAcquire("b") {
		t.Fatal("expected TryAcquire for 'b' to succeed")
	}
	if !r.IsRunning("a") || !r.IsRunning("b") {
		t.Error("both sessions should be running")
	}

	r.Release("a")
	if r.IsRunning("a") {
		t.Error("'a' should not be running after release")
	}
	if !r.IsRunning("b") {
		t.Error("'b' should still be running")
	}
}

func TestChatRunRegistry_ConcurrentAccess(t *testing.T) {
	r := NewChatRunRegistry()
	const sessionID = "concurrent"
	const goroutines = 100

	var wg sync.WaitGroup
	acquired := make(chan bool, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			acquired <- r.TryAcquire(sessionID)
		}()
	}

	wg.Wait()
	close(acquired)

	successCount := 0
	for ok := range acquired {
		if ok {
			successCount++
		}
	}

	if successCount != 1 {
		t.Errorf("expected exactly 1 successful acquire, got %d", successCount)
	}
}

func TestChatRunRegistry_IsRunningDefaultFalse(t *testing.T) {
	r := NewChatRunRegistry()
	if r.IsRunning("unknown") {
		t.Error("expected IsRunning = false for unknown session")
	}
}

func TestChatRunRegistry_ReleaseNonexistent(t *testing.T) {
	r := NewChatRunRegistry()
	// Should not panic.
	r.Release("nonexistent")
}

// ---------- WakeupDispatcher ----------

func TestWakeupDispatcher_RegisterAndWakeup(t *testing.T) {
	d := NewWakeupDispatcher()

	ch := d.Register("s1")

	d.Wakeup("s1")

	select {
	case <-ch:
		// success
	default:
		t.Error("expected wakeup signal on channel")
	}
}

func TestWakeupDispatcher_WakeupUnregistered(t *testing.T) {
	d := NewWakeupDispatcher()
	// Should not panic for an unregistered session.
	d.Wakeup("nonexistent")
}

func TestWakeupDispatcher_Unregister(t *testing.T) {
	d := NewWakeupDispatcher()

	ch := d.Register("s1")
	d.Unregister("s1")

	// Channel should be closed.
	_, ok := <-ch
	if ok {
		t.Error("expected channel to be closed after Unregister")
	}
}

func TestWakeupDispatcher_UnregisterNonexistent(t *testing.T) {
	d := NewWakeupDispatcher()
	// Should not panic.
	d.Unregister("nonexistent")
}

func TestWakeupDispatcher_WakeupBuffered(t *testing.T) {
	d := NewWakeupDispatcher()
	ch := d.Register("s1")

	// Multiple wakeups should not block (buffered channel of size 1).
	d.Wakeup("s1")
	d.Wakeup("s1")
	d.Wakeup("s1")

	// Drain the one buffered signal.
	select {
	case <-ch:
	default:
		t.Error("expected at least one wakeup signal")
	}

	// No more signals.
	select {
	case <-ch:
		t.Error("expected no more signals after drain")
	default:
	}
}

func TestWakeupDispatcher_ReRegister(t *testing.T) {
	d := NewWakeupDispatcher()

	ch1 := d.Register("s1")
	d.Unregister("s1")

	// Channel 1 should be closed.
	_, ok := <-ch1
	if ok {
		t.Error("expected ch1 to be closed")
	}

	// Re-register should provide a new channel.
	ch2 := d.Register("s1")
	d.Wakeup("s1")

	select {
	case <-ch2:
		// success
	default:
		t.Error("expected wakeup on re-registered channel")
	}
}

// ---------- WorkspaceManager ----------

func TestWorkspaceManager_GetOrCreate(t *testing.T) {
	dir := t.TempDir()
	mgr := NewWorkspaceManager(dir, "local")

	ws, err := mgr.GetOrCreate("session-1")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if ws == nil {
		t.Fatal("expected non-nil workspace")
	}
	if ws.BasePath() == "" {
		t.Error("expected non-empty BasePath")
	}

	// Second call should return the same workspace.
	ws2, err := mgr.GetOrCreate("session-1")
	if err != nil {
		t.Fatalf("GetOrCreate (second call): %v", err)
	}
	if ws != ws2 {
		t.Error("expected same workspace instance on second call")
	}
}

func TestWorkspaceManager_Get(t *testing.T) {
	dir := t.TempDir()
	mgr := NewWorkspaceManager(dir, "local")

	// Get before creation should return nil.
	if mgr.Get("session-1") != nil {
		t.Error("expected nil for non-created workspace")
	}

	_, _ = mgr.GetOrCreate("session-1")

	ws := mgr.Get("session-1")
	if ws == nil {
		t.Error("expected non-nil workspace after GetOrCreate")
	}
}

func TestWorkspaceManager_Remove(t *testing.T) {
	dir := t.TempDir()
	mgr := NewWorkspaceManager(dir, "local")

	_, _ = mgr.GetOrCreate("session-1")
	mgr.Remove("session-1")

	if mgr.Get("session-1") != nil {
		t.Error("expected nil after Remove")
	}
}

func TestWorkspaceManager_RemoveNonexistent(t *testing.T) {
	dir := t.TempDir()
	mgr := NewWorkspaceManager(dir, "local")
	// Should not panic.
	mgr.Remove("nonexistent")
}

func TestWorkspaceManager_List(t *testing.T) {
	dir := t.TempDir()
	mgr := NewWorkspaceManager(dir, "local")

	_, _ = mgr.GetOrCreate("s1")
	_, _ = mgr.GetOrCreate("s2")

	list := mgr.List()
	if len(list) != 2 {
		t.Fatalf("List() returned %d entries, want 2", len(list))
	}

	ids := make(map[string]bool)
	for _, info := range list {
		ids[info.SessionID] = true
		if info.Type != "local" {
			t.Errorf("workspace type = %q, want %q", info.Type, "local")
		}
		if info.BasePath == "" {
			t.Error("expected non-empty BasePath in listing")
		}
	}
	if !ids["s1"] || !ids["s2"] {
		t.Errorf("List() missing expected sessions, got IDs: %v", ids)
	}
}

func TestWorkspaceManager_UnsupportedBackend(t *testing.T) {
	dir := t.TempDir()
	mgr := NewWorkspaceManager(dir, "unsupported")

	_, err := mgr.GetOrCreate("s1")
	if err == nil {
		t.Error("expected error for unsupported backend")
	}
}

func TestWorkspaceManager_DefaultBackend(t *testing.T) {
	dir := t.TempDir()
	mgr := NewWorkspaceManager(dir, "")

	// Default backend should be "local".
	ws, err := mgr.GetOrCreate("s1")
	if err != nil {
		t.Fatalf("GetOrCreate with default backend: %v", err)
	}
	if ws == nil {
		t.Error("expected non-nil workspace with default backend")
	}
}

// ---------- BackgroundTaskManager ----------

func TestBackgroundTaskManager_TrackAndList(t *testing.T) {
	mgr := NewBackgroundTaskManager()

	_, cancel := context.WithCancel(context.Background())
	id := mgr.Track("session-1", cancel)

	if id == "" {
		t.Fatal("expected non-empty task ID")
	}

	tasks := mgr.List()
	if len(tasks) != 1 {
		t.Fatalf("List() returned %d tasks, want 1", len(tasks))
	}
	if tasks[0].ID != id {
		t.Errorf("task ID = %q, want %q", tasks[0].ID, id)
	}
	if tasks[0].SessionID != "session-1" {
		t.Errorf("session ID = %q, want %q", tasks[0].SessionID, "session-1")
	}
	if tasks[0].Status != "running" {
		t.Errorf("status = %q, want %q", tasks[0].Status, "running")
	}
}

func TestBackgroundTaskManager_Complete(t *testing.T) {
	mgr := NewBackgroundTaskManager()

	_, cancel := context.WithCancel(context.Background())
	id := mgr.Track("s1", cancel)

	mgr.Complete(id)

	tasks := mgr.List()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Status != "completed" {
		t.Errorf("status = %q, want %q", tasks[0].Status, "completed")
	}
}

func TestBackgroundTaskManager_Cancel(t *testing.T) {
	mgr := NewBackgroundTaskManager()

	ctx, cancel := context.WithCancel(context.Background())
	id := mgr.Track("s1", cancel)

	mgr.Cancel(id)

	// The underlying context should be canceled.
	select {
	case <-ctx.Done():
		// success
	default:
		t.Error("expected context to be canceled after Cancel")
	}

	tasks := mgr.List()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Status != "canceled" {
		t.Errorf("status = %q, want %q", tasks[0].Status, "canceled")
	}
}

func TestBackgroundTaskManager_CancelNonexistent(t *testing.T) {
	mgr := NewBackgroundTaskManager()
	// Should not panic.
	mgr.Cancel("nonexistent")
}

func TestBackgroundTaskManager_CompleteNonexistent(t *testing.T) {
	mgr := NewBackgroundTaskManager()
	// Should not panic.
	mgr.Complete("nonexistent")
}

func TestBackgroundTaskManager_MultipleTasks(t *testing.T) {
	mgr := NewBackgroundTaskManager()

	_, c1 := context.WithCancel(context.Background())
	_, c2 := context.WithCancel(context.Background())
	_, c3 := context.WithCancel(context.Background())

	id1 := mgr.Track("s1", c1)
	id2 := mgr.Track("s1", c2)
	id3 := mgr.Track("s2", c3)

	mgr.Complete(id1)
	mgr.Cancel(id3)

	tasks := mgr.List()
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(tasks))
	}

	status := make(map[string]string)
	for _, task := range tasks {
		status[task.ID] = task.Status
	}
	if status[id1] != "completed" {
		t.Errorf("task %s status = %q, want %q", id1, status[id1], "completed")
	}
	if status[id2] != "running" {
		t.Errorf("task %s status = %q, want %q", id2, status[id2], "running")
	}
	if status[id3] != "canceled" {
		t.Errorf("task %s status = %q, want %q", id3, status[id3], "canceled")
	}
}

// ---------- CancelDispatcher ----------

func TestCancelDispatcher_CancelBySession(t *testing.T) {
	bgMgr := NewBackgroundTaskManager()
	disp := NewCancelDispatcher(bgMgr)

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	ctx3, cancel3 := context.WithCancel(context.Background())

	bgMgr.Track("session-A", cancel1)
	bgMgr.Track("session-A", cancel2)
	bgMgr.Track("session-B", cancel3)

	// Cancel all tasks for session-A.
	disp.Cancel("session-A")

	// session-A tasks should be canceled.
	select {
	case <-ctx1.Done():
	default:
		t.Error("expected ctx1 to be canceled")
	}
	select {
	case <-ctx2.Done():
	default:
		t.Error("expected ctx2 to be canceled")
	}

	// session-B task should still be running.
	select {
	case <-ctx3.Done():
		t.Error("expected ctx3 to NOT be canceled")
	default:
	}

	// Verify statuses.
	tasks := bgMgr.List()
	for _, task := range tasks {
		if task.SessionID == "session-A" && task.Status != "canceled" {
			t.Errorf("session-A task %s status = %q, want %q", task.ID, task.Status, "canceled")
		}
		if task.SessionID == "session-B" && task.Status != "running" {
			t.Errorf("session-B task %s status = %q, want %q", task.ID, task.Status, "running")
		}
	}
}

func TestCancelDispatcher_CancelNoTasks(t *testing.T) {
	bgMgr := NewBackgroundTaskManager()
	disp := NewCancelDispatcher(bgMgr)

	// Should not panic when there are no tasks.
	disp.Cancel("nonexistent")
}

func TestCancelDispatcher_SkipsNonRunningTasks(t *testing.T) {
	bgMgr := NewBackgroundTaskManager()
	disp := NewCancelDispatcher(bgMgr)

	_, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())

	id1 := bgMgr.Track("s1", cancel1)
	bgMgr.Track("s1", cancel2)

	// Complete the first task.
	bgMgr.Complete(id1)

	// Cancel session should only affect the running task.
	disp.Cancel("s1")

	select {
	case <-ctx2.Done():
		// success - running task was canceled
	default:
		t.Error("expected running task context to be canceled")
	}

	// Verify: completed task stays completed, running task becomes canceled.
	for _, task := range bgMgr.List() {
		if task.ID == id1 && task.Status != "completed" {
			t.Errorf("completed task status changed to %q", task.Status)
		}
	}
}
