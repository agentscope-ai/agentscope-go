package middleware

import (
	"context"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

const DefaultBudgetHintMessage = "<system-reminder>You have reached the maximum token budget set by the " +
	"user. Now you MUST wrap up immediately and provide a final " +
	"concluding response without invoking any tools.</system-reminder>"

// ReplyBudgetControlMiddleware enforces a token budget per reply. When the
// accumulated weighted cost exceeds TokenBudget, it injects a hint message
// into the system prompt and forces tool_choice to "none" to make the agent
// produce a final text response.
//
// Cost per model call = InputTokenWeight * input_tokens + OutputTokenWeight * output_tokens.
type ReplyBudgetControlMiddleware struct {
	BaseMiddleware
	TokenBudget       float64
	InputTokenWeight  float64
	OutputTokenWeight float64
	HintMessage       string
}

// NewReplyBudgetControl creates a budget middleware with the given token limit
// and default weights (1.0 for both input and output tokens).
func NewReplyBudgetControl(tokenBudget float64) *ReplyBudgetControlMiddleware {
	return &ReplyBudgetControlMiddleware{
		BaseMiddleware:    BaseMiddleware{MiddlewareKey: "reply_budget_control"},
		TokenBudget:       tokenBudget,
		InputTokenWeight:  1,
		OutputTokenWeight: 1,
		HintMessage:       DefaultBudgetHintMessage,
	}
}

// OnReply resets the budget counter at the start of each reply to ensure
// per-reply isolation. Without this, budget from a previous reply (with a
// different MiddleContext) could leak if the same context were reused.
func (m *ReplyBudgetControlMiddleware) OnReply(ctx context.Context, input ReplyInput, next ReplyHandler) <-chan event.Event {
	if mc := GetMiddleContext(ctx); mc != nil && budgetState(ctx) == nil {
		mc.Set(m.Key(), "used", float64(0))
	}
	return next(ctx, input)
}

// OnModelCall tracks token usage after each model call and enforces the budget
// by forcing tool_choice to "none" when the limit is reached.
func (m *ReplyBudgetControlMiddleware) OnModelCall(
	ctx context.Context,
	input *ModelCallInput,
	next ModelCallHandler,
) (*model.ChatResponse, error) {
	mc := GetMiddleContext(ctx)

	if mc != nil && m.usedInContext(ctx, mc) >= m.TokenBudget {
		input.ToolChoice = &model.ToolChoice{Mode: "none"}
	}

	resp, err := next(ctx, input)
	if err != nil {
		return resp, err
	}

	if mc != nil && resp != nil && resp.Usage != nil {
		cost := m.InputTokenWeight*float64(resp.Usage.InputTokens) +
			m.OutputTokenWeight*float64(resp.Usage.OutputTokens)
		m.addInContext(ctx, mc, cost)
	}

	return resp, nil
}

// OnSystemPrompt appends a budget warning to the system prompt when the token
// budget has been reached.
func (m *ReplyBudgetControlMiddleware) OnSystemPrompt(
	ctx context.Context,
	_ string,
	currentPrompt string,
) string {
	mc := GetMiddleContext(ctx)
	if mc != nil && m.usedInContext(ctx, mc) >= m.TokenBudget {
		return currentPrompt + "\n\n" + m.HintMessage
	}
	return currentPrompt
}

func (m *ReplyBudgetControlMiddleware) getUsed(mc MiddleContext) float64 {
	if v, ok := mc.Get(m.Key(), "used"); ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return 0
}

func (m *ReplyBudgetControlMiddleware) addUsed(mc MiddleContext, cost float64) {
	mc.Set(m.Key(), "used", m.getUsed(mc)+cost)
}
