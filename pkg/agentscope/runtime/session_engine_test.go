package runtime

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestSessionEngineSubmitMessage(t *testing.T) {
	mc := &seMockModelCaller{
		resp: &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "response"}},
			IsLast:  true,
		},
	}

	se := NewSessionEngine(SessionEngineConfig{
		LoopOptions: []loop.Option{loop.WithModelCaller(mc), loop.WithMaxIters(1)},
	})

	var events []event.Event
	for ev := range se.SubmitMessage(context.Background(), "hello") {
		events = append(events, ev)
	}

	if len(events) == 0 {
		t.Fatal("expected events")
	}

	hasReplyEnd := false
	for _, ev := range events {
		if ev.GetEventType() == event.EventReplyEnd {
			hasReplyEnd = true
		}
	}
	if !hasReplyEnd {
		t.Error("missing ReplyEnd event")
	}
}

func TestSessionEnginePreservesHistoryAcrossTurns(t *testing.T) {
	mc := &historyModelCaller{}
	se := NewSessionEngine(SessionEngineConfig{
		LoopOptions: []loop.Option{
			loop.WithModelCaller(mc),
			loop.WithMaxIters(1),
			loop.WithSystemPrompt("stay concise"),
		},
	})

	for range se.SubmitMessage(context.Background(), "first") {
	}
	for range se.SubmitMessage(context.Background(), "second") {
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()
	if len(mc.calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(mc.calls))
	}
	got := mc.calls[1]
	wantRoles := []message.Role{
		message.RoleSystem,
		message.RoleUser,
		message.RoleAssistant,
		message.RoleUser,
	}
	if len(got) != len(wantRoles) {
		t.Fatalf("second-turn history has %d messages, want %d", len(got), len(wantRoles))
	}
	for i, want := range wantRoles {
		if got[i].Role != want {
			t.Errorf("history[%d].Role = %q, want %q", i, got[i].Role, want)
		}
	}
	if state := se.State(); len(state.Messages) != 5 {
		t.Fatalf("persisted history has %d messages, want 5", len(state.Messages))
	}
}

func TestSessionEngineUsesConfiguredContextManager(t *testing.T) {
	cm := loop.NewDefaultContextManager()
	cm.Append(message.UserMsg("user", "restored history"))
	mc := &historyModelCaller{}
	se := NewSessionEngine(SessionEngineConfig{
		LoopOptions: []loop.Option{
			loop.WithModelCaller(mc),
			loop.WithContextManager(cm),
		},
	})
	for range se.SubmitMessage(context.Background(), "first") {
	}
	for range se.SubmitMessage(context.Background(), "second") {
	}
	if got := len(cm.Messages()); got != 5 {
		t.Fatalf("configured manager has %d messages, want restored history and two turns", got)
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if len(mc.calls) != 2 || len(mc.calls[0]) != 2 || len(mc.calls[1]) != 4 {
		t.Fatalf("model did not receive restored and accumulated history: %v", mc.calls)
	}
}

func TestSessionEngineID(t *testing.T) {
	se := NewSessionEngine(SessionEngineConfig{})
	if se.ID() == "" {
		t.Error("SessionEngine ID should not be empty")
	}
}

func TestSessionEngineInterrupt(t *testing.T) {
	// Create a model caller that blocks until context is canceled
	mc := &blockingModelCaller{done: make(chan struct{})}
	se := NewSessionEngine(SessionEngineConfig{
		LoopOptions: []loop.Option{loop.WithModelCaller(mc), loop.WithMaxIters(10)},
	})

	ch := se.SubmitMessage(context.Background(), "long task")

	// Wait a bit then interrupt
	time.Sleep(10 * time.Millisecond)
	se.Interrupt()

	// Drain events — should complete without hanging
	for range ch {
	}
	close(mc.done)
	// Test passes if it doesn't hang
}

func TestSessionEngineBudgetEnforcement(t *testing.T) {
	mc := &seMockModelCaller{
		resp: &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}},
			IsLast:  true,
		},
	}

	se := NewSessionEngine(SessionEngineConfig{
		LoopOptions: []loop.Option{loop.WithModelCaller(mc), loop.WithMaxIters(1)},
		Budget:      Budget{MaxTurns: 1},
	})

	// First turn should succeed
	for range se.SubmitMessage(context.Background(), "first") {
	}

	// Second turn should hit budget limit
	var events []event.Event
	for ev := range se.SubmitMessage(context.Background(), "second") {
		events = append(events, ev)
	}

	hasBudgetEvent := false
	for _, ev := range events {
		if ce, ok := ev.(event.CustomEvent); ok && ce.Name == "turn.budget_exceeded" {
			hasBudgetEvent = true
		}
	}
	if !hasBudgetEvent {
		t.Error("expected budget exceeded event on second turn")
	}
}

func TestSessionEngineSessionHooks(t *testing.T) {
	mc := &seMockModelCaller{
		resp: &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}},
			IsLast:  true,
		},
	}

	se := NewSessionEngine(SessionEngineConfig{
		LoopOptions: []loop.Option{loop.WithModelCaller(mc), loop.WithMaxIters(1)},
	})

	var fired []SessionHookEvent
	se.Hooks().Register(&FuncHook{
		Fn:   func(e SessionHookEvent, _ any) error { fired = append(fired, e); return nil },
		Evts: []SessionHookEvent{HookSessionStart, HookSessionEnd, HookPreTurn, HookPostTurn},
	})

	for range se.SubmitMessage(context.Background(), "test") {
	}

	expected := []SessionHookEvent{HookSessionStart, HookPreTurn, HookPostTurn, HookSessionEnd}
	if len(fired) != len(expected) {
		t.Fatalf("fired %d hooks, want %d: %v", len(fired), len(expected), fired)
	}
	for i, want := range expected {
		if fired[i] != want {
			t.Errorf("fired[%d] = %v, want %v", i, fired[i], want)
		}
	}
}

func TestSessionEngineStatePersisted(t *testing.T) {
	mc := &seMockModelCaller{
		resp: &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}},
			IsLast:  true,
		},
	}

	store := NewInMemorySessionStore()
	se := NewSessionEngine(SessionEngineConfig{
		LoopOptions: []loop.Option{loop.WithModelCaller(mc), loop.WithMaxIters(1)},
		Store:       store,
	})

	for range se.SubmitMessage(context.Background(), "test") {
	}

	loaded, err := store.Load(se.ID())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if loaded.ID != se.ID() {
		t.Errorf("persisted ID = %q, want %q", loaded.ID, se.ID())
	}
}

func TestSessionEngineState(t *testing.T) {
	se := NewSessionEngine(SessionEngineConfig{})
	state := se.State()
	if state.ID != se.ID() {
		t.Errorf("State().ID = %q, want %q", state.ID, se.ID())
	}
	if state.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}
}

// --- Mocks ---

type seMockModelCaller struct {
	resp *model.ChatResponse
}

func (m *seMockModelCaller) Call(_ context.Context, _ []*message.Msg, _ []model.ToolSchema) (*model.ChatResponse, error) {
	return m.resp, nil
}

type historyModelCaller struct {
	mu    sync.Mutex
	calls [][]*message.Msg
}

func (m *historyModelCaller) Call(_ context.Context, messages []*message.Msg, _ []model.ToolSchema) (*model.ChatResponse, error) {
	m.mu.Lock()
	m.calls = append(m.calls, append([]*message.Msg(nil), messages...))
	n := len(m.calls)
	m.mu.Unlock()
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: fmt.Sprintf("response %d", n)}},
		IsLast:  true,
	}, nil
}

type blockingModelCaller struct {
	done chan struct{}
}

func (m *blockingModelCaller) Call(ctx context.Context, _ []*message.Msg, _ []model.ToolSchema) (*model.ChatResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.done:
		return &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}},
			IsLast:  true,
		}, nil
	}
}
