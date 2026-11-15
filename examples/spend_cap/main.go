package main

import (
	"context"
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// mockModel simulates a model that reports token usage on every call.
type mockModel struct {
	callCount int
}

func (m *mockModel) Chat(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	m.callCount++
	return &model.ChatResponse{
		Content: []message.ContentBlock{
			message.TextBlock{Type: "text", Text: fmt.Sprintf("Response #%d from the model.", m.callCount)},
		},
		ModelName: "gpt-4o-mini",
		Usage: &model.ChatUsage{
			InputTokens:  500,
			OutputTokens: 200,
		},
	}, nil
}

func (m *mockModel) ChatStream(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (m *mockModel) CountTokens(_ []*message.Msg, _ []model.ToolSchema) int { return 0 }

func main() {
	fmt.Println("=== Spend Cap (CostTrackerMiddleware) Example ===")
	fmt.Println()
	ctx := context.Background()

	// Define per-model pricing (USD per million tokens).
	prices := map[string]middleware.ModelPrice{
		"gpt-4o-mini": {
			InputPerMillion:  0.15, // $0.15 per 1M input tokens
			OutputPerMillion: 0.60, // $0.60 per 1M output tokens
		},
	}

	// Create CostTracker with a $0.01 spend cap and CNY exchange rate.
	tracker := middleware.NewCostTrackerMiddleware(prices,
		middleware.WithMaxCostUSD(0.01),
		middleware.WithExchangeRate("CNY", 7.2),
	)

	mock := &mockModel{}

	// Each call costs: 500 * 0.15/1e6 + 200 * 0.60/1e6 = $0.000195
	// Budget of $0.01 allows ~51 calls. After 52 are recorded, budget is exceeded.
	fmt.Printf("Budget: $0.0100 (per call cost: ~$0.000195)\n")
	fmt.Printf("Making calls until budget exceeded...\n\n")

	var budgetExceeded bool
	for i := 1; i <= 60; i++ {
		tracker.NewTurn()

		// Create a fresh agent each iteration so Reply() cannot return a
		// stale assistant message from a prior successful call.
		a := agent.NewUnifiedAgent("spender", "You are helpful.", mock,
			agent.WithMiddlewares(tracker),
			agent.WithReactConfig(agent.ReactConfig{MaxIters: 1}),
		)

		prevCount := mock.callCount
		_, err := a.Reply(ctx, fmt.Sprintf("Question %d", i))

		// Detect budget enforcement: if the model was never called, the
		// middleware blocked it pre-flight.
		if mock.callCount == prevCount || err != nil {
			cost := tracker.GetSessionCost()
			fmt.Printf("  Call %d: BLOCKED (budget exceeded at $%.6f)\n", i, cost.TotalCostUSD)
			budgetExceeded = true
			break
		}

		if i <= 3 || i%10 == 0 || i == 52 {
			cost := tracker.GetSessionCost()
			fmt.Printf("  Call %d: OK  (total: $%.6f)\n", i, cost.TotalCostUSD)
		}
	}

	fmt.Println()

	// Print final session cost summary.
	sc := tracker.GetSessionCost()
	fmt.Println("--- Session Cost Summary ---")
	fmt.Printf("  Total Input Tokens:  %d\n", sc.TotalInputTokens)
	fmt.Printf("  Total Output Tokens: %d\n", sc.TotalOutputTokens)
	fmt.Printf("  Total Cost (USD):    $%.6f\n", sc.TotalCostUSD)
	for currency, amount := range sc.ConvertedCosts {
		fmt.Printf("  Total Cost (%s):    ¥%.4f\n", currency, amount)
	}
	fmt.Printf("  Models used:\n")
	for name, mc := range sc.ByModel {
		fmt.Printf("    %s: in=%d out=%d cost=$%.6f\n", name, mc.InputTokens, mc.OutputTokens, mc.CostUSD)
	}
	fmt.Printf("  Turns tracked: %d\n", len(sc.ByTurn))

	if budgetExceeded {
		fmt.Println()
		fmt.Println("Budget was enforced successfully!")
	}

	fmt.Println()
	fmt.Println("=== Done ===")
}
