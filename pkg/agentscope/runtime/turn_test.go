package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestTurnForwardsEvents(t *testing.T) {
	mc := &turnMockModelCaller{
		resp: &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "hello"}},
			IsLast:  true,
		},
	}
	l := loop.New(loop.WithModelCaller(mc), loop.WithMaxIters(1))

	turn := NewTurn(TurnConfig{Loop: l})
	var events []event.Event
	for ev := range turn.Run(context.Background(), "hi") {
		events = append(events, ev)
	}

	if len(events) == 0 {
		t.Fatal("expected events from turn")
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

func TestTurnFiresHooks(t *testing.T) {
	mc := &turnMockModelCaller{
		resp: &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}},
			IsLast:  true,
		},
	}
	l := loop.New(loop.WithModelCaller(mc), loop.WithMaxIters(1))

	hooks := NewSessionHookManager()
	var fired []SessionHookEvent
	hooks.Register(&FuncHook{
		Fn:   func(e SessionHookEvent, _ any) error { fired = append(fired, e); return nil },
		Evts: []SessionHookEvent{HookPreTurn, HookPostTurn},
	})

	turn := NewTurn(TurnConfig{Loop: l, Hooks: hooks})
	for ev := range turn.Run(context.Background(), "test") {
		_ = ev
	}

	if len(fired) != 2 {
		t.Fatalf("got %d hooks fired, want 2", len(fired))
	}
	if fired[0] != HookPreTurn {
		t.Errorf("first hook = %v, want HookPreTurn", fired[0])
	}
	if fired[1] != HookPostTurn {
		t.Errorf("second hook = %v, want HookPostTurn", fired[1])
	}
}

func TestTurnBudgetExceeded(t *testing.T) {
	mc := &turnMockModelCaller{
		resp: &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}},
			IsLast:  true,
		},
	}
	l := loop.New(loop.WithModelCaller(mc), loop.WithMaxIters(1))
	bt := NewBudgetTracker(Budget{MaxTurns: 1})
	bt.AddTurn() // exhaust the budget

	turn := NewTurn(TurnConfig{Loop: l, Budget: bt})
	var events []event.Event
	for ev := range turn.Run(context.Background(), "should fail") {
		events = append(events, ev)
	}

	hasBudgetEvent := false
	for _, ev := range events {
		if ce, ok := ev.(event.CustomEvent); ok && ce.Name == "turn.budget_exceeded" {
			hasBudgetEvent = true
		}
	}
	if !hasBudgetEvent {
		t.Error("expected turn.budget_exceeded event")
	}
}

func TestTurnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	mc := &turnMockModelCaller{
		resp: &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "never"}},
			IsLast:  true,
		},
	}
	l := loop.New(loop.WithModelCaller(mc), loop.WithMaxIters(1))

	turn := NewTurn(TurnConfig{Loop: l})
	var events []event.Event
	for ev := range turn.Run(ctx, "canceled") {
		events = append(events, ev)
	}
	// Should complete without hanging
	if len(events) == 0 {
		t.Log("no events (expected with canceled context)")
	}
}

func TestTurnID(t *testing.T) {
	turn := NewTurn(TurnConfig{})
	if turn.ID() == "" {
		t.Error("Turn ID should not be empty")
	}
}

func TestTurnNilLoop(t *testing.T) {
	turn := NewTurn(TurnConfig{})
	var events []event.Event
	for ev := range turn.Run(context.Background(), "test") {
		events = append(events, ev)
	}

	hasError := false
	for _, ev := range events {
		if ce, ok := ev.(event.CustomEvent); ok && ce.Name == "turn.error" {
			hasError = true
		}
	}
	if !hasError {
		t.Error("expected turn.error event when loop is nil")
	}
}

func TestTurnBudgetWaitsForHistoryMutation(t *testing.T) {
	cm := &gatedContextManager{
		DefaultContextManager: loop.NewDefaultContextManager(),
		entered:               make(chan struct{}),
		release:               make(chan struct{}),
	}
	mc := &turnMockModelCaller{resp: &model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "response"}},
		Usage:   &model.ChatUsage{InputTokens: 10},
	}}
	turn := NewTurn(TurnConfig{
		Loop:   loop.New(loop.WithModelCaller(mc), loop.WithContextManager(cm)),
		Budget: NewBudgetTracker(Budget{MaxTokens: 1}),
	})
	done := make(chan struct{})
	budgetExceeded := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range turn.Run(context.Background(), "test") {
			if ce, ok := ev.(event.CustomEvent); ok && ce.Name == "turn.budget_exceeded" {
				close(budgetExceeded)
			}
		}
	}()
	defer func() {
		close(cm.release)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("turn did not stop after releasing history mutation")
		}
	}()
	select {
	case <-cm.entered:
	case <-time.After(time.Second):
		t.Fatal("loop did not reach history mutation")
	}
	select {
	case <-budgetExceeded:
	case <-time.After(time.Second):
		t.Fatal("token budget did not stop the turn")
	}
	select {
	case <-done:
		t.Fatal("turn returned while the loop was still mutating shared history")
	case <-time.After(50 * time.Millisecond):
	}
}

type gatedContextManager struct {
	*loop.DefaultContextManager
	entered chan struct{}
	release chan struct{}
}

func (m *gatedContextManager) Append(msg *message.Msg) {
	if msg.Role == message.RoleAssistant {
		close(m.entered)
		<-m.release
	}
	m.DefaultContextManager.Append(msg)
}

// --- Mock ---

type turnMockModelCaller struct {
	resp *model.ChatResponse
}

func (m *turnMockModelCaller) Call(_ context.Context, _ []*message.Msg, _ []model.ToolSchema) (*model.ChatResponse, error) {
	return m.resp, nil
}
