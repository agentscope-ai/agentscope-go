package agent

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// A completed reply emits exactly one turn_stats event, aggregating every
// model call and tool execution of the turn (codex token-count parity).
func TestTurnStats_CompletedReply(t *testing.T) {
	mock := &mockChatModel{responses: []model.ChatResponse{
		{Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "tc_s", Name: "echo_a", Input: `{}`},
		}, IsLast: true, Usage: &model.ChatUsage{InputTokens: 100, OutputTokens: 10}},
		{Content: []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "all done"},
		}, IsLast: true, Usage: &model.ChatUsage{InputTokens: 120, OutputTokens: 8}},
	}}
	a := NewUnifiedAgent("stats-agent", "helpful", mock,
		WithToolkit(tool.NewToolkit(echoToolFixtureA())),
		WithPermissionContext(permission.NewContext(permission.ModeBypass)),
		WithReactConfig(ReactConfig{MaxIters: 4}),
	)
	a.state.SessionID = "sess-stats"

	ch, err := a.ReplyStream(context.Background(), "use the tool")
	if err != nil {
		t.Fatal(err)
	}
	var statsEvents []event.CustomEvent
	var ended bool
	for evt := range ch {
		switch e := evt.(type) {
		case event.CustomEvent:
			if e.Name == "turn_stats" {
				statsEvents = append(statsEvents, e)
			}
		case event.ReplyEndEvent:
			ended = true
			if e.FinishedReason != types.ReplyCompleted {
				t.Fatalf("reply ended with %s: %+v", e.FinishedReason, e.Error)
			}
		}
	}
	if !ended {
		t.Fatal("missing ReplyEndEvent")
	}
	if len(statsEvents) != 1 {
		t.Fatalf("expected exactly one turn_stats event, got %d", len(statsEvents))
	}
	v := statsEvents[0].Value
	if v["model_calls"] != 2 {
		t.Errorf("model_calls = %v, want 2", v["model_calls"])
	}
	if v["tool_calls"] != 1 {
		t.Errorf("tool_calls = %v, want 1", v["tool_calls"])
	}
	if v["input_tokens"] != 220 || v["output_tokens"] != 18 {
		t.Errorf("token sums wrong: in=%v out=%v", v["input_tokens"], v["output_tokens"])
	}
	if v["iterations"] != 2 {
		t.Errorf("iterations = %v, want 2", v["iterations"])
	}
	if v["last_input_tokens"] != 120 {
		t.Errorf("last_input_tokens = %v, want 120", v["last_input_tokens"])
	}
}

// A budget-exhausted reply still reports its usage, counting both the
// tool-calling iteration and the forced finalization model call: hosts
// must be able to account tokens burned before the failure.
func TestTurnStats_ErrorReply(t *testing.T) {
	mock := &mockChatModel{responses: []model.ChatResponse{
		{Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "tc_x", Name: "nonexistent_tool", Input: `{}`},
		}, IsLast: true, Usage: &model.ChatUsage{InputTokens: 50, OutputTokens: 4}},
	}}
	a := NewUnifiedAgent("stats-agent", "helpful", mock,
		WithToolkit(tool.NewToolkit(echoToolFixtureA())),
		WithPermissionContext(permission.NewContext(permission.ModeBypass)),
		WithReactConfig(ReactConfig{MaxIters: 1}),
	)
	a.state.SessionID = "sess-stats-err"

	ch, err := a.ReplyStream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	var stats *event.CustomEvent
	for evt := range ch {
		if e, ok := evt.(event.CustomEvent); ok && e.Name == "turn_stats" {
			e := e
			stats = &e
		}
	}
	if stats == nil {
		t.Fatal("missing turn_stats on failed reply")
	}
	if stats.Value["model_calls"] != 2 || stats.Value["input_tokens"] != 50 {
		t.Errorf("partial usage lost: %+v", stats.Value)
	}
}
