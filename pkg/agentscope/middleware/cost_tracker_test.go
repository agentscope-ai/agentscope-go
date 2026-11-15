package middleware

import (
	"context"
	"errors"
	"math"
	"testing"

	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestCostTracker_BasicAccumulation(t *testing.T) {
	prices := map[string]ModelPrice{
		"gpt-4o": {InputPerMillion: 5.0, OutputPerMillion: 15.0},
	}
	ct := NewCostTrackerMiddleware(prices)

	handler := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			ModelName: "gpt-4o",
			Usage:     &model.ChatUsage{InputTokens: 100, OutputTokens: 50},
			IsLast:    true,
		}, nil
	}

	// Make two calls.
	for i := 0; i < 2; i++ {
		_, err := ct.OnModelCall(context.Background(), &ModelCallInput{}, handler)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	sc := ct.GetSessionCost()
	if sc.TotalInputTokens != 200 {
		t.Errorf("TotalInputTokens = %d, want 200", sc.TotalInputTokens)
	}
	if sc.TotalOutputTokens != 100 {
		t.Errorf("TotalOutputTokens = %d, want 100", sc.TotalOutputTokens)
	}
	// Expected cost per call: 100*5/1e6 + 50*15/1e6 = 0.0005 + 0.00075 = 0.00125
	// Two calls = 0.0025
	expectedCost := 0.0025
	if math.Abs(sc.TotalCostUSD-expectedCost) > 1e-9 {
		t.Errorf("TotalCostUSD = %f, want %f", sc.TotalCostUSD, expectedCost)
	}
	mc, ok := sc.ByModel["gpt-4o"]
	if !ok {
		t.Fatal("expected gpt-4o in ByModel")
	}
	if mc.InputTokens != 200 || mc.OutputTokens != 100 {
		t.Errorf("ByModel tokens: input=%d output=%d", mc.InputTokens, mc.OutputTokens)
	}
}

func TestCostTracker_BudgetExceeded(t *testing.T) {
	prices := map[string]ModelPrice{
		"gpt-4o": {InputPerMillion: 5.0, OutputPerMillion: 15.0},
	}
	// Per-call cost = 100*5/1e6 + 50*15/1e6 = 0.00125
	// Set budget to allow exactly one call.
	ct := NewCostTrackerMiddleware(prices, WithMaxCostUSD(0.002))

	handler := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			ModelName: "gpt-4o",
			Usage:     &model.ChatUsage{InputTokens: 100, OutputTokens: 50},
			IsLast:    true,
		}, nil
	}

	// First call should succeed.
	_, err := ct.OnModelCall(context.Background(), &ModelCallInput{}, handler)
	if err != nil {
		t.Fatalf("first call should succeed: %v", err)
	}

	// Second call: cost (0.00125) now >= budget (0.002) is false, but
	// after second call cost becomes 0.0025 >= 0.002, so third call
	// should be rejected.
	_, err = ct.OnModelCall(context.Background(), &ModelCallInput{}, handler)
	if err != nil {
		t.Fatalf("second call should succeed: %v", err)
	}

	// Third call: accumulated cost 0.0025 >= budget 0.002 → rejected.
	_, err = ct.OnModelCall(context.Background(), &ModelCallInput{}, handler)
	if err == nil {
		t.Fatal("expected budget exceeded error")
	}
	if !errors.Is(err, agenterrors.ErrBudgetExceeded) {
		t.Errorf("expected errors.Is(err, ErrBudgetExceeded), got %T: %v", err, err)
	}
}

func TestCostTracker_ExchangeRate(t *testing.T) {
	prices := map[string]ModelPrice{
		"gpt-4o": {InputPerMillion: 5.0, OutputPerMillion: 15.0},
	}
	ct := NewCostTrackerMiddleware(prices, WithExchangeRate("CNY", 7.2))

	handler := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			ModelName: "gpt-4o",
			Usage:     &model.ChatUsage{InputTokens: 1_000_000, OutputTokens: 0},
			IsLast:    true,
		}, nil
	}

	_, err := ct.OnModelCall(context.Background(), &ModelCallInput{}, handler)
	if err != nil {
		t.Fatal(err)
	}

	sc := ct.GetSessionCost()
	// 1M input tokens * 5 USD/M = 5 USD
	if math.Abs(sc.TotalCostUSD-5.0) > 1e-9 {
		t.Errorf("TotalCostUSD = %f, want 5.0", sc.TotalCostUSD)
	}
	cny, ok := sc.ConvertedCosts["CNY"]
	if !ok {
		t.Fatal("expected CNY in ConvertedCosts")
	}
	if math.Abs(cny-36.0) > 1e-9 {
		t.Errorf("ConvertedCosts[CNY] = %f, want 36.0", cny)
	}
}

func TestCostTracker_TurnTracking(t *testing.T) {
	prices := map[string]ModelPrice{
		"gpt-4o": {InputPerMillion: 5.0, OutputPerMillion: 15.0},
	}
	ct := NewCostTrackerMiddleware(prices)

	handler := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			ModelName: "gpt-4o",
			Usage:     &model.ChatUsage{InputTokens: 100, OutputTokens: 50},
			IsLast:    true,
		}, nil
	}

	ct.NewTurn()
	_, _ = ct.OnModelCall(context.Background(), &ModelCallInput{}, handler)
	_, _ = ct.OnModelCall(context.Background(), &ModelCallInput{}, handler)

	ct.NewTurn()
	_, _ = ct.OnModelCall(context.Background(), &ModelCallInput{}, handler)

	sc := ct.GetSessionCost()
	if len(sc.ByTurn) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(sc.ByTurn))
	}
	if sc.ByTurn[0].InputTokens != 200 {
		t.Errorf("turn 1 input tokens = %d, want 200", sc.ByTurn[0].InputTokens)
	}
	if sc.ByTurn[1].InputTokens != 100 {
		t.Errorf("turn 2 input tokens = %d, want 100", sc.ByTurn[1].InputTokens)
	}
}

func TestCostTracker_UnknownModel(t *testing.T) {
	prices := map[string]ModelPrice{
		"gpt-4o": {InputPerMillion: 5.0, OutputPerMillion: 15.0},
	}
	ct := NewCostTrackerMiddleware(prices)

	handler := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			ModelName: "unknown-model-xyz",
			Usage:     &model.ChatUsage{InputTokens: 500, OutputTokens: 200},
			IsLast:    true,
		}, nil
	}

	_, err := ct.OnModelCall(context.Background(), &ModelCallInput{}, handler)
	if err != nil {
		t.Fatal(err)
	}

	sc := ct.GetSessionCost()
	if sc.TotalInputTokens != 500 {
		t.Errorf("TotalInputTokens = %d, want 500", sc.TotalInputTokens)
	}
	if sc.TotalOutputTokens != 200 {
		t.Errorf("TotalOutputTokens = %d, want 200", sc.TotalOutputTokens)
	}
	// Unknown model → zero cost.
	if sc.TotalCostUSD != 0 {
		t.Errorf("TotalCostUSD = %f, want 0 for unknown model", sc.TotalCostUSD)
	}
}

func TestCostTracker_NilUsage(t *testing.T) {
	ct := NewCostTrackerMiddleware(nil)

	handler := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			ModelName: "gpt-4o",
			Usage:     nil, // nil usage
			IsLast:    true,
		}, nil
	}

	_, err := ct.OnModelCall(context.Background(), &ModelCallInput{}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sc := ct.GetSessionCost()
	if sc.TotalInputTokens != 0 || sc.TotalOutputTokens != 0 || sc.TotalCostUSD != 0 {
		t.Errorf("expected zero totals for nil usage, got input=%d output=%d cost=%f",
			sc.TotalInputTokens, sc.TotalOutputTokens, sc.TotalCostUSD)
	}
}
