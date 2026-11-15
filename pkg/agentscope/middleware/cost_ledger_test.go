package middleware

import (
	"context"
	"testing"
	"time"

	"errors"
	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestCostLedger_RecordAndSummary(t *testing.T) {
	l := NewCostLedger()
	l.Record(&LedgerEntry{SessionID: "s1", AgentName: "a", ModelName: "gpt-4o", InputTokens: 1000, OutputTokens: 500, CostUSD: 0.01, Timestamp: time.Now()})
	l.Record(&LedgerEntry{SessionID: "s2", AgentName: "a", ModelName: "gpt-4o-mini", InputTokens: 2000, OutputTokens: 100, CostUSD: 0.002, Timestamp: time.Now()})

	all := l.Summary(CostFilter{})
	if all.Calls != 2 || all.TotalCostUSD != 0.012 {
		t.Fatalf("all summary wrong: %+v", all)
	}
	s1 := l.Summary(CostFilter{SessionID: "s1"})
	if s1.Calls != 1 || s1.TotalCostUSD != 0.01 {
		t.Fatalf("s1 summary wrong: %+v", s1)
	}
	if got := s1.ByModel["gpt-4o"]; got != 0.01 {
		t.Errorf("ByModel = %v", s1.ByModel)
	}
}

func TestPriceResolveAndCost(t *testing.T) {
	p, ok := model.ResolvePrice("gpt-4o")
	if !ok || p.Input <= 0 || p.Output <= 0 {
		t.Fatalf("built-in overlay missing gpt-4o: %+v %v", p, ok)
	}
	// prefix match for versioned names
	if _, ok := model.ResolvePrice("claude-sonnet-4-5-20260101"); !ok {
		t.Error("prefix match failed for versioned model name")
	}
	// override wins
	model.SetPrice("gpt-4o", model.Price{Input: 1, Output: 2})
	p2, _ := model.ResolvePrice("gpt-4o")
	if p2.Input != 1 || p2.Output != 2 {
		t.Errorf("override not honored: %+v", p2)
	}
	cost := p2.CostUSD(&model.ChatUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if cost != 3.0 {
		t.Errorf("cost = %v, want 3.0", cost)
	}
}

func usageHandler(in, out int) ModelCallHandler {
	return func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{Usage: &model.ChatUsage{InputTokens: in, OutputTokens: out}}, nil
	}
}

func TestCostTracking_RecordsIntoLedger(t *testing.T) {
	l := NewCostLedger()
	m := NewCostTracking(l, "sess-1", "agent-x", map[string]model.Price{
		"test-model": {Input: 1.0, Output: 2.0}, // $1/$2 per M
	})
	ctx := context.Background()
	_, err := m.OnModelCall(ctx, &ModelCallInput{ModelName: "test-model"}, usageHandler(2_000_000, 1_000_000))
	if err != nil {
		t.Fatal(err)
	}
	s := l.Summary(CostFilter{SessionID: "sess-1"})
	if s.Calls != 1 {
		t.Fatalf("calls = %d", s.Calls)
	}
	// 2M*1/1M + 1M*2/1M = 4
	if s.TotalCostUSD != 4.0 {
		t.Errorf("cost = %v, want 4.0", s.TotalCostUSD)
	}
}

func TestReplyCostBudget_SoftWarnAndHardStop(t *testing.T) {
	prices := map[string]model.Price{"m1": {Input: 1000.0, Output: 0}} // $1000/M input → 100k tokens = $100
	m := NewReplyCostBudget(100.0, WithCostBudgetPrices(prices), WithCostBudgetWarnRatio(0.8))

	mc := MiddleContext{}
	ctx := WithMiddleContext(context.Background(), mc)

	// Reply setup resets state.
	m.OnReply(ctx, ReplyInput{AgentName: "a"}, func(_ context.Context, _ ReplyInput) <-chan event.Event { return nil })

	// First call: $100 spent — past both warn and hard cap for NEXT call.
	_, err := m.OnModelCall(ctx, &ModelCallInput{ModelName: "m1"}, usageHandler(100_000, 0))
	if err != nil {
		t.Fatalf("first call should pass: %v", err)
	}
	// Warning flag set → hint injected.
	prompt := m.OnSystemPrompt(ctx, "a", "base")
	if prompt == "base" {
		t.Error("soft warning hint not injected at/above warn ratio")
	}
	// Second call: already at cap → hard stop with typed error.
	_, err = m.OnModelCall(ctx, &ModelCallInput{ModelName: "m1"}, usageHandler(1, 0))
	if !errors.Is(err, agenterrors.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got %v", err)
	}
}
