package middleware

import (
	"context"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func budgetCtx() context.Context {
	return WithMiddleContext(context.Background(), MiddleContext{})
}

func modelCallCore(inputTokens, outputTokens int) ModelCallHandler {
	return func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}},
			Usage:   &model.ChatUsage{InputTokens: inputTokens, OutputTokens: outputTokens},
		}, nil
	}
}

func TestBudget_UnderBudget_NoEnforcement(t *testing.T) {
	m := NewReplyBudgetControl(1000)
	ctx := budgetCtx()

	// Model call with small usage
	resp, err := m.OnModelCall(ctx, &ModelCallInput{}, modelCallCore(10, 5))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetTextContent() != "ok" {
		t.Error("response should pass through")
	}

	// System prompt should be unchanged
	prompt := m.OnSystemPrompt(ctx, "agent", "Base prompt.")
	if prompt != "Base prompt." {
		t.Error("prompt should not contain hint when under budget")
	}
}

func TestBudget_ExactlyMet_Enforces(t *testing.T) {
	m := NewReplyBudgetControl(100)
	ctx := budgetCtx()

	// First call: uses exactly 100 tokens (50+50)
	_, err := m.OnModelCall(ctx, &ModelCallInput{}, modelCallCore(50, 50))
	if err != nil {
		t.Fatal(err)
	}

	// Now budget is exactly met (100 >= 100)
	prompt := m.OnSystemPrompt(ctx, "agent", "Base.")
	if !strings.Contains(prompt, "maximum token budget") {
		t.Error("hint should be injected when budget is met")
	}
}

func TestBudget_ZeroBudget_ImmediateEnforcement(t *testing.T) {
	m := NewReplyBudgetControl(0)
	ctx := budgetCtx()

	// Even before any model call, budget=0 >= 0 = used
	prompt := m.OnSystemPrompt(ctx, "agent", "Base.")
	if !strings.Contains(prompt, "maximum token budget") {
		t.Error("zero budget should enforce immediately")
	}

	// OnModelCall should force tool_choice=none
	var receivedInput ModelCallInput
	core := func(_ context.Context, input *ModelCallInput) (*model.ChatResponse, error) {
		receivedInput = *input
		return &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}},
		}, nil
	}

	_, err := m.OnModelCall(ctx, &ModelCallInput{
		ToolChoice: &model.ToolChoice{Mode: "auto"},
	}, core)
	if err != nil {
		t.Fatal(err)
	}

	if receivedInput.ToolChoice == nil || receivedInput.ToolChoice.Mode != "none" {
		t.Errorf("expected tool_choice=none, got %+v", receivedInput.ToolChoice)
	}
}

func TestBudget_ForcesToolChoiceNone(t *testing.T) {
	m := NewReplyBudgetControl(50)
	ctx := budgetCtx()

	// First call: uses 60 tokens, exceeding budget
	_, _ = m.OnModelCall(ctx, &ModelCallInput{}, modelCallCore(30, 30))

	// Second call: should force tool_choice=none
	var receivedInput ModelCallInput
	core := func(_ context.Context, input *ModelCallInput) (*model.ChatResponse, error) {
		receivedInput = *input
		return &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "wrap up"}},
		}, nil
	}

	_, err := m.OnModelCall(ctx, &ModelCallInput{
		ToolChoice: &model.ToolChoice{Mode: "auto"},
	}, core)
	if err != nil {
		t.Fatal(err)
	}

	if receivedInput.ToolChoice == nil || receivedInput.ToolChoice.Mode != "none" {
		t.Error("should force tool_choice=none when over budget")
	}
}

func TestBudget_AccumulatesAcrossCalls(t *testing.T) {
	m := NewReplyBudgetControl(300)
	ctx := budgetCtx()

	// Call 1: 100 tokens
	_, _ = m.OnModelCall(ctx, &ModelCallInput{}, modelCallCore(60, 40))

	prompt1 := m.OnSystemPrompt(ctx, "agent", "Base.")
	if strings.Contains(prompt1, "maximum token budget") {
		t.Error("should not enforce after 100 tokens")
	}

	// Call 2: 100 tokens (total 200)
	_, _ = m.OnModelCall(ctx, &ModelCallInput{}, modelCallCore(50, 50))

	prompt2 := m.OnSystemPrompt(ctx, "agent", "Base.")
	if strings.Contains(prompt2, "maximum token budget") {
		t.Error("should not enforce after 200 tokens")
	}

	// Call 3: 100 tokens (total 300, exactly budget)
	_, _ = m.OnModelCall(ctx, &ModelCallInput{}, modelCallCore(70, 30))

	prompt3 := m.OnSystemPrompt(ctx, "agent", "Base.")
	if !strings.Contains(prompt3, "maximum token budget") {
		t.Error("should enforce after 300 tokens (exactly budget)")
	}
}

func TestBudget_WeightedCost(t *testing.T) {
	m := NewReplyBudgetControl(200)
	m.InputTokenWeight = 1
	m.OutputTokenWeight = 3
	ctx := budgetCtx()

	// 50 input + 50 output = 50*1 + 50*3 = 200 weighted cost
	_, _ = m.OnModelCall(ctx, &ModelCallInput{}, modelCallCore(50, 50))

	prompt := m.OnSystemPrompt(ctx, "agent", "Base.")
	if !strings.Contains(prompt, "maximum token budget") {
		t.Error("weighted cost 200 should meet budget of 200")
	}
}

func TestBudget_WeightedCost_UnderBudget(t *testing.T) {
	m := NewReplyBudgetControl(200)
	m.InputTokenWeight = 1
	m.OutputTokenWeight = 3
	ctx := budgetCtx()

	// 50 input + 40 output = 50*1 + 40*3 = 170 weighted cost
	_, _ = m.OnModelCall(ctx, &ModelCallInput{}, modelCallCore(50, 40))

	prompt := m.OnSystemPrompt(ctx, "agent", "Base.")
	if strings.Contains(prompt, "maximum token budget") {
		t.Error("weighted cost 170 should be under budget of 200")
	}
}

func TestBudget_CustomHintMessage(t *testing.T) {
	m := NewReplyBudgetControl(0)
	m.HintMessage = "CUSTOM WARNING"
	ctx := budgetCtx()

	prompt := m.OnSystemPrompt(ctx, "agent", "Base.")
	if !strings.Contains(prompt, "CUSTOM WARNING") {
		t.Error("should use custom hint message")
	}
}

func TestBudget_NilUsage_NoAccumulation(t *testing.T) {
	m := NewReplyBudgetControl(100)
	ctx := budgetCtx()

	core := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}},
			Usage:   nil,
		}, nil
	}

	_, _ = m.OnModelCall(ctx, &ModelCallInput{}, core)

	mc := GetMiddleContext(ctx)
	used := m.getUsed(mc)
	if used != 0 {
		t.Errorf("nil usage should not accumulate, got %f", used)
	}
}

func TestBudget_ModelCallError_NoAccumulation(t *testing.T) {
	m := NewReplyBudgetControl(100)
	ctx := budgetCtx()

	core := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return nil, context.Canceled
	}

	_, err := m.OnModelCall(ctx, &ModelCallInput{}, core)
	if err == nil {
		t.Error("expected error from core")
	}

	mc := GetMiddleContext(ctx)
	used := m.getUsed(mc)
	if used != 0 {
		t.Errorf("error should not accumulate, got %f", used)
	}
}

func TestBudget_NoMiddleContext_Passthrough(t *testing.T) {
	m := NewReplyBudgetControl(100)
	ctx := context.Background() // no MiddleContext

	resp, err := m.OnModelCall(ctx, &ModelCallInput{}, modelCallCore(50, 50))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetTextContent() != "ok" {
		t.Error("should pass through when no MiddleContext")
	}

	prompt := m.OnSystemPrompt(ctx, "agent", "Base.")
	if prompt != "Base." {
		t.Error("should not modify prompt when no MiddleContext")
	}
}

func TestBudget_Key(t *testing.T) {
	m := NewReplyBudgetControl(100)
	if m.Key() != "reply_budget_control" {
		t.Errorf("expected key reply_budget_control, got %s", m.Key())
	}
}

func TestBudget_PerReplyIsolation(t *testing.T) {
	m := NewReplyBudgetControl(100)

	// Reply 1
	ctx1 := budgetCtx()
	_, _ = m.OnModelCall(ctx1, &ModelCallInput{}, modelCallCore(60, 60))
	prompt1 := m.OnSystemPrompt(ctx1, "agent", "Base.")
	if !strings.Contains(prompt1, "maximum token budget") {
		t.Error("reply 1 should exceed budget")
	}

	// Reply 2 (fresh MiddleContext)
	ctx2 := budgetCtx()
	prompt2 := m.OnSystemPrompt(ctx2, "agent", "Base.")
	if strings.Contains(prompt2, "maximum token budget") {
		t.Error("reply 2 should start fresh, not inherit reply 1's budget")
	}
}

func TestBudget_OnReply_ResetsBudget(t *testing.T) {
	m := NewReplyBudgetControl(100)
	ctx := budgetCtx()

	// Accumulate cost past the budget.
	_, _ = m.OnModelCall(ctx, &ModelCallInput{}, modelCallCore(60, 60))
	mc := GetMiddleContext(ctx)
	if m.getUsed(mc) == 0 {
		t.Fatal("expected non-zero usage after model call")
	}

	// OnReply should reset the counter.
	next := func(ctx context.Context, _ ReplyInput) <-chan event.Event {
		// Verify that usage is reset inside the reply handler.
		innerMC := GetMiddleContext(ctx)
		if m.getUsed(innerMC) != 0 {
			t.Error("budget should be reset to 0 inside OnReply")
		}
		ch := make(chan event.Event)
		close(ch)
		return ch
	}

	ch := m.OnReply(ctx, ReplyInput{AgentName: "test"}, next)
	for range ch {
	}

	// After OnReply, the budget should be reset.
	if m.getUsed(mc) != 0 {
		t.Errorf("budget should be 0 after OnReply reset, got %f", m.getUsed(mc))
	}
}

func TestBudget_OnReply_NoMiddleContext_Passthrough(t *testing.T) {
	m := NewReplyBudgetControl(100)
	ctx := context.Background() // no MiddleContext

	called := false
	next := func(_ context.Context, _ ReplyInput) <-chan event.Event {
		called = true
		ch := make(chan event.Event)
		close(ch)
		return ch
	}

	ch := m.OnReply(ctx, ReplyInput{}, next)
	for range ch {
	}

	if !called {
		t.Error("next should be called even without MiddleContext")
	}
}
