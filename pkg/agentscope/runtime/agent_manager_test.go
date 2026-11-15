package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

type amMockChatModel struct {
	delay time.Duration
}

func (m *amMockChatModel) Chat(ctx context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}},
		IsLast:  true,
	}, nil
}

func (m *amMockChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	resp, err := m.Chat(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	ch := make(chan model.ChatResponse, 1)
	ch <- *resp
	close(ch)
	return ch, nil
}

func (m *amMockChatModel) CountTokens(_ []*message.Msg, _ []model.ToolSchema) int {
	return 0
}

func TestAgentManagerSpawnSync(t *testing.T) {
	am := NewAgentManager(nil, nil)
	mock := &amMockChatModel{}

	ma, err := am.Spawn(context.Background(), mock, &AgentConfig{
		Name: "test-sync",
	}, "say hello")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if ma.Status != AgentStatusDone {
		t.Errorf("Status = %v, want Done", ma.Status)
	}
	if ma.Result == nil {
		t.Error("Result should not be nil")
	}
	if ma.FinishedAt.IsZero() {
		t.Error("FinishedAt should be set")
	}
}

func TestAgentManagerSpawnBackground(t *testing.T) {
	am := NewAgentManager(nil, nil)
	mock := &amMockChatModel{delay: 100 * time.Millisecond}

	ma, err := am.Spawn(context.Background(), mock, &AgentConfig{
		Name:       "test-bg",
		Background: true,
		Timeout:    5 * time.Second,
	}, "background task")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Should return immediately while agent is still running
	ma.mu.Lock()
	status := ma.Status
	ma.mu.Unlock()
	if status != AgentStatusRunning {
		t.Errorf("Status = %v immediately after spawn, want Running", status)
	}

	// Wait should block then complete
	if err := ma.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	ma.mu.Lock()
	finalStatus := ma.Status
	ma.mu.Unlock()
	if finalStatus != AgentStatusDone {
		t.Errorf("Status after wait = %v, want Done", finalStatus)
	}
}

func TestAgentManagerStop(t *testing.T) {
	am := NewAgentManager(nil, nil)
	mock := &amMockChatModel{delay: 10 * time.Second}

	ma, err := am.Spawn(context.Background(), mock, &AgentConfig{
		Name:       "test-stop",
		Background: true,
		Timeout:    30 * time.Second,
	}, "long task")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if err := am.Stop(ma.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	ma.mu.Lock()
	status := ma.Status
	ma.mu.Unlock()
	if status != AgentStatusStopped {
		t.Errorf("Status = %v, want Stopped", status)
	}
}

func TestAgentManagerList(t *testing.T) {
	am := NewAgentManager(nil, nil)
	mock := &amMockChatModel{}

	am.Spawn(context.Background(), mock, &AgentConfig{Name: "a1"}, "task1")
	am.Spawn(context.Background(), mock, &AgentConfig{Name: "a2"}, "task2")

	infos := am.List()
	if len(infos) != 2 {
		t.Fatalf("List returned %d agents, want 2", len(infos))
	}

	names := map[string]bool{}
	for _, info := range infos {
		names[info.Name] = true
	}
	if !names["a1"] || !names["a2"] {
		t.Errorf("List names = %v, want a1 and a2", names)
	}
}

func TestAgentManagerBudgetLimit(t *testing.T) {
	bt := NewBudgetTracker(Budget{MaxConcurrency: 1})
	am := NewAgentManager(bt, nil)
	mock := &amMockChatModel{delay: 1 * time.Second}

	// First background agent should succeed
	_, err := am.Spawn(context.Background(), mock, &AgentConfig{
		Name:       "first",
		Background: true,
		Timeout:    5 * time.Second,
	}, "task1")
	if err != nil {
		t.Fatalf("first Spawn: %v", err)
	}

	// Second should fail due to budget
	_, err = am.Spawn(context.Background(), mock, &AgentConfig{
		Name:       "second",
		Background: true,
		Timeout:    5 * time.Second,
	}, "task2")
	if err == nil {
		t.Fatal("second Spawn should have failed due to budget limit")
	}

	am.StopAll()
}

func TestAgentManagerHooks(t *testing.T) {
	hooks := NewSessionHookManager()
	am := NewAgentManager(nil, hooks)
	mock := &amMockChatModel{}

	var mu sync.Mutex
	var fired []SessionHookEvent
	hooks.Register(&FuncHook{
		Fn: func(e SessionHookEvent, _ any) error {
			mu.Lock()
			fired = append(fired, e)
			mu.Unlock()
			return nil
		},
		Evts: []SessionHookEvent{HookSubagentStart, HookSubagentEnd},
	})

	am.Spawn(context.Background(), mock, &AgentConfig{Name: "hooked"}, "task")

	mu.Lock()
	defer mu.Unlock()

	if len(fired) != 2 {
		t.Fatalf("fired %d hooks, want 2: %v", len(fired), fired)
	}
	if fired[0] != HookSubagentStart {
		t.Errorf("fired[0] = %v, want SubagentStart", fired[0])
	}
	if fired[1] != HookSubagentEnd {
		t.Errorf("fired[1] = %v, want SubagentEnd", fired[1])
	}
}

func TestAgentManagerStopAll(t *testing.T) {
	am := NewAgentManager(nil, nil)
	mock := &amMockChatModel{delay: 10 * time.Second}

	for i := 0; i < 3; i++ {
		am.Spawn(context.Background(), mock, &AgentConfig{
			Name:       "bg",
			Background: true,
			Timeout:    30 * time.Second,
		}, "task")
	}

	if am.ActiveCount() != 3 {
		t.Fatalf("ActiveCount = %d, want 3", am.ActiveCount())
	}

	am.StopAll()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	am.WaitAll(ctx)

	if am.ActiveCount() != 0 {
		t.Errorf("ActiveCount after StopAll = %d, want 0", am.ActiveCount())
	}
}
