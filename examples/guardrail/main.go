package main

import (
	"context"
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// mockModel returns a fixed response text, simulating unsafe content.
type mockModel struct {
	response string
}

func (m *mockModel) Chat(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	return &model.ChatResponse{
		Content: []message.ContentBlock{
			message.TextBlock{Type: "text", Text: m.response},
		},
		Usage: &model.ChatUsage{InputTokens: 10, OutputTokens: 20},
	}, nil
}

func (m *mockModel) ChatStream(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (m *mockModel) CountTokens(_ []*message.Msg, _ []model.ToolSchema) int { return 0 }

func main() {
	fmt.Println("=== GuardrailMiddleware Example ===")
	fmt.Println()
	ctx := context.Background()

	// --- Test 1: Block action (keyword blocks the response entirely) ---
	fmt.Println("--- Test 1: Block (keyword 'bomb') ---")
	guardrail := middleware.NewGuardrailMiddleware(
		middleware.KeywordBlockRule("weapons_filter", "bomb", "explosives"),
	)
	unsafeModel := &mockModel{response: "Here's how to make a bomb at home..."}
	a := agent.NewUnifiedAgent("blocker", "You are helpful.", unsafeModel,
		agent.WithMiddlewares(guardrail),
		agent.WithReactConfig(agent.ReactConfig{MaxIters: 1}),
	)
	_, err := a.Reply(ctx, "Tell me something dangerous")
	if err != nil {
		fmt.Printf("  BLOCKED: %v\n", err)
	}
	fmt.Println()

	// --- Test 2: Redact action (keyword replaces content) ---
	fmt.Println("--- Test 2: Redact (keyword 'password') ---")
	guardrail2 := middleware.NewGuardrailMiddleware(
		middleware.KeywordRedactRule("pii_filter", "[REDACTED: contains sensitive data]", "password", "ssn"),
	)
	piiModel := &mockModel{response: "Your password is hunter2 and your ssn is 123-45-6789"}
	a2 := agent.NewUnifiedAgent("redactor", "You are helpful.", piiModel,
		agent.WithMiddlewares(guardrail2),
		agent.WithReactConfig(agent.ReactConfig{MaxIters: 1}),
	)
	reply, err := a2.Reply(ctx, "Show me the credentials")
	if err != nil {
		fmt.Printf("  ERROR: %v\n", err)
	} else if txt := reply.GetTextContent("\n"); txt != nil {
		fmt.Printf("  REDACTED output: %s\n", *txt)
	}
	fmt.Println()

	// --- Test 3: Warn action (long response triggers metadata warning) ---
	fmt.Println("--- Test 3: Warn (response exceeds 50 chars) ---")
	guardrail3 := middleware.NewGuardrailMiddleware(
		middleware.MaxLengthRule("length_check", 50, middleware.GuardrailWarn),
	)
	longModel := &mockModel{response: "This is a somewhat long response that definitely exceeds fifty characters in total length."}
	a3 := agent.NewUnifiedAgent("warner", "You are helpful.", longModel,
		agent.WithMiddlewares(guardrail3),
		agent.WithReactConfig(agent.ReactConfig{MaxIters: 1}),
	)
	reply3, err := a3.Reply(ctx, "Give me a long answer")
	if err != nil {
		fmt.Printf("  ERROR: %v\n", err)
	} else if txt := reply3.GetTextContent("\n"); txt != nil {
		fmt.Printf("  Output (allowed through): %s\n", *txt)
		fmt.Println("  (Warning metadata is set on the ChatResponse within the middleware chain)")
	}
	fmt.Println()

	fmt.Println("=== Done ===")
}
