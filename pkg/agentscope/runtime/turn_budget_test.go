package runtime

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// TestTurnTokenBudgetEnforced proves the token budget (previously dead config on
// the loop path) is enforced: once a model call's usage pushes the tracker over
// MaxTokens, the turn emits a budget-exceeded event and records the tokens.
func TestTurnTokenBudgetEnforced(t *testing.T) {
	mc := &turnMockModelCaller{
		resp: &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "hi"}},
			IsLast:  true,
			Usage:   &model.ChatUsage{InputTokens: 100, OutputTokens: 50},
		},
	}
	l := loop.New(loop.WithModelCaller(mc), loop.WithMaxIters(5))
	bt := NewBudgetTracker(Budget{MaxTokens: 10}) // 150 tokens >> 10

	turn := NewTurn(TurnConfig{Loop: l, Budget: bt})

	sawBudgetEvent := false
	for ev := range turn.Run(context.Background(), "hi") {
		if ce, ok := ev.(event.CustomEvent); ok && ce.Name == "turn.budget_exceeded" {
			sawBudgetEvent = true
		}
	}
	if !sawBudgetEvent {
		t.Fatal("expected a turn.budget_exceeded event once token usage exceeded MaxTokens")
	}
	if bt.TokensUsed() == 0 {
		t.Error("expected tokens to be recorded on the tracker")
	}
}
