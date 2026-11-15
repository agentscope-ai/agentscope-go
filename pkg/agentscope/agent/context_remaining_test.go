package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func TestContextRemainingTool_EndToEnd(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 100, contextSize: 100000}
	mock.chatResponse = &model.ChatResponse{
		Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "tc_cr", Name: "get_context_remaining", Input: `{}`, State: message.ToolCallPending},
		},
		IsLast: true,
	}
	a := NewUnifiedAgent("budget-agent", "helpful", mock,
		WithToolkit(tool.NewToolkit()),
		WithContextRemainingTool(),
		WithContextConfig(&ContextConfig{ContextSize: 100000}),
		WithReactConfig(ReactConfig{MaxIters: 2}),
	)

	ch, err := a.ReplyStream(context.Background(), "check budget")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	var resultText string
	for _, m := range a.state.Context {
		for _, b := range m.Content {
			if tr, ok := b.(message.ToolResultBlock); ok {
				resultText, _ = tr.Output.(string)
			}
		}
	}
	if !strings.Contains(resultText, `"tokens_left"`) {
		t.Fatalf("expected tokens_left payload, got %q", resultText)
	}
	// 100000 window minus a ~100-token estimate must leave a large budget.
	if !strings.Contains(resultText, `"tokens_left":99`) {
		t.Errorf("expected ~99k tokens left, got %q", resultText)
	}
}

func TestContextRemainingTool_Decreases(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 100, contextSize: 100000}
	a := NewUnifiedAgent("budget-agent", "helpful", mock,
		WithContextConfig(&ContextConfig{ContextSize: 100000}),
	)
	left1, ok := a.contextRemainingForTool(context.Background())
	if !ok {
		t.Fatal("estimate unavailable")
	}
	mock.tokenCount = 90000
	left2, _ := a.contextRemainingForTool(context.Background())
	if left2 >= left1 {
		t.Errorf("budget did not shrink with context: %d -> %d", left1, left2)
	}
}

// A provider-reported usage spike grounds the estimate when the byte
// heuristic under-counts (the same grounding the compression trigger uses).
func TestContextRemainingTool_UsageGrounded(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 100, contextSize: 100000}
	a := NewUnifiedAgent("budget-agent", "helpful", mock,
		WithContextConfig(&ContextConfig{ContextSize: 100000}),
	)
	a.lastModelInputTokens = 80000
	left, _ := a.contextRemainingForTool(context.Background())
	if left != 20000 {
		t.Errorf("usage-grounded estimate = %d left, want 20000", left)
	}
}
