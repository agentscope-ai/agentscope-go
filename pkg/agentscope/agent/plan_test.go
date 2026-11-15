package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func planCallResponse(id, input string) model.ChatResponse {
	return model.ChatResponse{
		Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: id, Name: "update_plan", Input: input, State: message.ToolCallPending},
		},
		IsLast: true,
	}
}

const twoStepPlanInput = `{"explanation":"start work","plan":[{"step":"design","status":"completed"},{"step":"implement","status":"in_progress"},{"step":"test","status":"pending"}]}`

func TestUpdatePlanTool_EndToEnd(t *testing.T) {
	mock := &mockChatModel{responses: []model.ChatResponse{
		planCallResponse("tc_p1", twoStepPlanInput),
		finalTextResponse("plan noted"),
	}}
	a := NewUnifiedAgent("plan-agent", "helpful", mock,
		WithToolkit(tool.NewToolkit()),
		WithPlanTool(),
		WithReactConfig(ReactConfig{MaxIters: 4}),
	)

	ch, err := a.ReplyStream(context.Background(), "plan this")
	if err != nil {
		t.Fatal(err)
	}
	var planEvent map[string]any
	for evt := range ch {
		if ce, ok := evt.(event.CustomEvent); ok && ce.Name == "plan_updated" {
			planEvent = ce.Value
		}
	}

	// State recorded.
	if len(a.state.Plan) != 3 {
		t.Fatalf("plan not recorded: %+v", a.state.Plan)
	}
	if a.state.Plan[1].Status != tool.PlanStatusInProgress {
		t.Errorf("step 2 should be in_progress: %+v", a.state.Plan[1])
	}

	// Live-render event emitted with the plan payload.
	if planEvent == nil {
		t.Fatal("plan_updated event not emitted")
	}
	items, ok := planEvent["plan"].([]any)
	if !ok || len(items) != 3 {
		t.Fatalf("event plan payload wrong: %+v", planEvent)
	}
	if planEvent["explanation"] != "start work" {
		t.Errorf("explanation missing from event: %+v", planEvent)
	}

	// Injected into the model input so the tracker survives across calls.
	found := false
	for _, m := range a.prepareModelInput(context.Background()) {
		for _, b := range m.Content {
			if tb, ok := b.(message.TextBlock); ok && strings.Contains(tb.Text, "[Current plan]") && strings.Contains(tb.Text, "implement") {
				found = true
			}
		}
	}
	if !found {
		t.Error("plan not injected into model input")
	}
}

func TestUpdatePlanTool_ValidationRejected(t *testing.T) {
	mock := &mockChatModel{responses: []model.ChatResponse{
		planCallResponse("tc_p1", `{"plan":[{"step":"a","status":"in_progress"},{"step":"b","status":"in_progress"}]}`),
		finalTextResponse("fixed"),
	}}
	a := NewUnifiedAgent("plan-agent", "helpful", mock,
		WithToolkit(tool.NewToolkit()),
		WithPlanTool(),
		WithReactConfig(ReactConfig{MaxIters: 4}),
	)
	ch, err := a.ReplyStream(context.Background(), "plan")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if len(a.state.Plan) != 0 {
		t.Errorf("invalid plan must not be recorded: %+v", a.state.Plan)
	}
	// The error went back to the model as a tool result it can act on.
	var resultText string
	for _, m := range a.state.Context {
		for _, b := range m.Content {
			if tr, ok := b.(message.ToolResultBlock); ok {
				resultText, _ = tr.Output.(string)
			}
		}
	}
	if !strings.Contains(resultText, "at most one") {
		t.Errorf("expected validation error surfaced to model, got %q", resultText)
	}
}

func TestUpdatePlanTool_EmptyPlanClears(t *testing.T) {
	a := NewUnifiedAgent("plan-agent", "helpful", &mockChatModel{},
		WithToolkit(tool.NewToolkit()),
		WithPlanTool(),
	)
	if err := a.updatePlanFromTool(context.Background(), "", []tool.PlanItem{{Step: "x", Status: "pending"}}); err != nil {
		t.Fatal(err)
	}
	if len(a.state.Plan) != 1 {
		t.Fatal("plan not set")
	}
	if err := a.updatePlanFromTool(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}
	if len(a.state.Plan) != 0 {
		t.Errorf("empty plan should clear the tracker: %+v", a.state.Plan)
	}
}

func TestPlanCheckpointRoundTrip(t *testing.T) {
	saver := newFakeStateSaver()
	a := NewUnifiedAgent("plan-agent", "helpful", &mockChatModel{},
		WithToolkit(tool.NewToolkit()),
		WithPlanTool(),
		WithStateSaver(saver),
	)
	a.state.SessionID = "sess-plan"
	if err := a.updatePlanFromTool(context.Background(), "", []tool.PlanItem{
		{Step: "design", Status: tool.PlanStatusCompleted},
		{Step: "implement", Status: tool.PlanStatusInProgress},
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveCheckpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCheckpoint(context.Background(), saver, "sess-plan")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Plan) != 2 || loaded.Plan[1].Step != "implement" {
		t.Fatalf("plan lost across checkpoint: %+v", loaded.Plan)
	}
}

// The plan lives outside Context precisely so compression cannot eat it.
func TestPlanSurvivesCompression(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 5000, contextSize: 32000}
	a := NewUnifiedAgent("plan-agent", "helpful", mock,
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8, ReserveRatio: 0.1}),
		WithPlanTool(),
	)
	for i := 0; i < 10; i++ {
		a.state.Context = append(a.state.Context,
			message.UserMsg("user", "message"),
			message.AssistantMsg("plan-agent", "reply"),
		)
	}
	if err := a.updatePlanFromTool(context.Background(), "", []tool.PlanItem{
		{Step: "finish the task", Status: tool.PlanStatusInProgress},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(a.state.Plan) != 1 {
		t.Fatalf("plan lost in compression: %+v", a.state.Plan)
	}
	found := false
	for _, m := range a.prepareModelInput(context.Background()) {
		for _, b := range m.Content {
			if tb, ok := b.(message.TextBlock); ok && strings.Contains(tb.Text, "finish the task") {
				found = true
			}
		}
	}
	if !found {
		t.Error("plan missing from model input after compression")
	}
}

// Two update_plan calls in one model response run sequentially (the tool is
// not concurrency-safe: plan updates are ordered state transitions), emit
// their live-render events in order, and the last write wins in state.
func TestUpdatePlanTool_SequentialOrderEmitsEvents(t *testing.T) {
	mock := &mockChatModel{responses: []model.ChatResponse{
		{Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "tc_a", Name: "update_plan",
				Input: `{"plan":[{"step":"one","status":"in_progress"}]}`, State: message.ToolCallPending},
			message.ToolCallBlock{Type: "tool_call", ID: "tc_b", Name: "update_plan",
				Input: `{"plan":[{"step":"one","status":"completed"},{"step":"two","status":"in_progress"}]}`, State: message.ToolCallPending},
		}, IsLast: true},
		finalTextResponse("done"),
	}}
	a := NewUnifiedAgent("plan-agent", "helpful", mock,
		WithToolkit(tool.NewToolkit()),
		WithPlanTool(),
		WithReactConfig(ReactConfig{MaxIters: 4}),
	)
	ch, err := a.ReplyStream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for evt := range ch {
		if ce, ok := evt.(event.CustomEvent); ok && ce.Name == "plan_updated" {
			events = append(events, ce.Value)
		}
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 plan_updated events, got %d", len(events))
	}
	// Last write wins in state.
	if len(a.state.Plan) != 2 || a.state.Plan[1].Status != tool.PlanStatusInProgress {
		t.Errorf("final plan state wrong: %+v", a.state.Plan)
	}
}
