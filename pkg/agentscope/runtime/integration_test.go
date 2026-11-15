package runtime

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agenttest"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/metrics"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tracing"
)

// TestIntegrationSessionEngineWithHooks wires together MetricsHook,
// TracingHook, ScriptedModelCaller, Loop, and SessionEngine, then verifies
// the event stream, session persistence, and budget tracking all work as a
// system.
func TestIntegrationSessionEngineWithHooks(t *testing.T) {
	metricsHook := metrics.NewMetricsHook(metrics.Noop())
	tracingHook := tracing.NewTracingHook(tracing.NoopTracer{})

	mc := agenttest.NewScriptedModelCaller(&model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "Hello!"}},
		IsLast:  true,
	})

	store := NewInMemorySessionStore()
	se := NewSessionEngine(SessionEngineConfig{
		LoopOptions: []loop.Option{
			loop.WithModelCaller(mc),
			loop.WithMaxIters(1),
			loop.WithHooks(metricsHook, tracingHook),
		},
		Store: store,
	})

	var events []event.Event
	for ev := range se.SubmitMessage(context.Background(), "hi") {
		events = append(events, ev)
	}

	if len(events) == 0 {
		t.Fatal("expected events but got none")
	}

	// Verify ReplyStart and ReplyEnd events are present.
	hasReplyStart := false
	hasReplyEnd := false
	for _, ev := range events {
		switch ev.GetEventType() {
		case event.EventReplyStart:
			hasReplyStart = true
		case event.EventReplyEnd:
			hasReplyEnd = true
		}
	}
	if !hasReplyStart {
		t.Error("missing ReplyStart event")
	}
	if !hasReplyEnd {
		t.Error("missing ReplyEnd event")
	}

	// Verify session state was persisted.
	loaded, err := store.Load(se.ID())
	if err != nil {
		t.Fatalf("store.Load(%q) failed: %v", se.ID(), err)
	}
	if loaded.ID != se.ID() {
		t.Errorf("persisted ID = %q, want %q", loaded.ID, se.ID())
	}

	// Verify budget tracker recorded 1 turn.
	if got := se.Budget().TurnsUsed(); got != 1 {
		t.Errorf("TurnsUsed() = %d, want 1", got)
	}
}

// TestIntegrationBudgetExceededAcrossTurns verifies that the budget tracker
// correctly enforces turn limits across multiple SubmitMessage calls.
func TestIntegrationBudgetExceededAcrossTurns(t *testing.T) {
	mc := agenttest.NewScriptedModelCaller(
		&model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "resp1"}},
			IsLast:  true,
		},
		&model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "resp2"}},
			IsLast:  true,
		},
		&model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "resp3"}},
			IsLast:  true,
		},
	)

	metricsHook := metrics.NewMetricsHook(metrics.Noop())
	tracingHook := tracing.NewTracingHook(tracing.NoopTracer{})

	se := NewSessionEngine(SessionEngineConfig{
		LoopOptions: []loop.Option{
			loop.WithModelCaller(mc),
			loop.WithMaxIters(1),
			loop.WithHooks(metricsHook, tracingHook),
		},
		Budget: Budget{MaxTurns: 2},
	})

	// Turn 1: should succeed.
	for range se.SubmitMessage(context.Background(), "first") {
	}

	// Turn 2: should succeed.
	for range se.SubmitMessage(context.Background(), "second") {
	}

	// Turn 3: should get budget_exceeded.
	var events []event.Event
	for ev := range se.SubmitMessage(context.Background(), "third") {
		events = append(events, ev)
	}

	hasBudgetExceeded := false
	for _, ev := range events {
		if ce, ok := ev.(event.CustomEvent); ok && ce.Name == "turn.budget_exceeded" {
			hasBudgetExceeded = true
		}
	}
	if !hasBudgetExceeded {
		t.Error("expected turn.budget_exceeded CustomEvent on third turn")
	}

	if !se.Budget().Exceeded() {
		t.Error("Budget().Exceeded() should be true after exceeding MaxTurns")
	}
}

// TestIntegrationToolExecution wires a tool-calling model response through the
// full SessionEngine pipeline and verifies that the tool executor is invoked
// and the proper events are emitted.
func TestIntegrationToolExecution(t *testing.T) {
	toolCallResp := &model.ChatResponse{
		Content: []message.ContentBlock{
			message.ToolCallBlock{
				Type:  "tool_use",
				ID:    "tc1",
				Name:  "bash",
				Input: `{"command":"ls"}`,
				State: message.ToolCallPending,
			},
		},
		IsLast: true,
	}
	textResp := &model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "Done."}},
		IsLast:  true,
	}

	mc := agenttest.NewScriptedModelCaller(toolCallResp, textResp)
	te := agenttest.NewSimpleToolExecutor("tool-output")

	se := NewSessionEngine(SessionEngineConfig{
		LoopOptions: []loop.Option{
			loop.WithModelCaller(mc),
			loop.WithToolExecutor(te),
			loop.WithMaxIters(5),
		},
	})

	var events []event.Event
	for ev := range se.SubmitMessage(context.Background(), "run ls") {
		events = append(events, ev)
	}

	if len(events) == 0 {
		t.Fatal("expected events but got none")
	}

	// Verify at least one ToolResultEnd event.
	hasToolResultEnd := false
	for _, ev := range events {
		if ev.GetEventType() == event.EventToolResultEnd {
			hasToolResultEnd = true
		}
	}
	if !hasToolResultEnd {
		t.Error("expected at least one ToolResultEnd event")
	}

	// Verify tool executor was called exactly once.
	if got := te.CallCount(); got != 1 {
		t.Errorf("tool executor CallCount() = %d, want 1", got)
	}
}
