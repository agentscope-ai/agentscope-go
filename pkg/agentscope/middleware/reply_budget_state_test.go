package middleware

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestReplyBudgetSnapshotResumeAndOwnership(t *testing.T) {
	token := NewReplyBudgetControl(10)
	cost := NewReplyCostBudget(1, WithCostBudgetPrices(map[string]model.Price{"fixture": {Input: 1000000}}))
	snapshot := &ReplyBudgetSnapshot{Version: 1, Counters: map[string]ReplyBudgetCounter{token.Key(): {UsedTokens: 10}, cost.Key(): {SpentUSD: 0.9, Warned: true}}}
	ctx, err := WithReplyBudgetSnapshot(WithMiddleContext(context.Background(), MiddleContext{}), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Counters[token.Key()] = ReplyBudgetCounter{}
	nextReply := func(context.Context, ReplyInput) <-chan event.Event { return nil }
	token.OnReply(ctx, ReplyInput{}, nextReply)
	cost.OnReply(ctx, ReplyInput{}, nextReply)
	called := false
	_, err = token.OnModelCall(ctx, &ModelCallInput{}, func(_ context.Context, in *ModelCallInput) (*model.ChatResponse, error) {
		called = true
		if in.ToolChoice == nil || in.ToolChoice.Mode != "none" {
			t.Fatal("restored token budget reset")
		}
		return &model.ChatResponse{Usage: &model.ChatUsage{InputTokens: 2}}, nil
	})
	if err != nil || !called {
		t.Fatal(err)
	}
	saved := SnapshotReplyBudgets(ctx)
	if saved.Counters[token.Key()].UsedTokens != 12 || saved.Counters[cost.Key()].SpentUSD != 0.9 {
		t.Fatalf("budget not retained: %+v", saved)
	}
	saved.Counters[token.Key()] = ReplyBudgetCounter{}
	if SnapshotReplyBudgets(ctx).Counters[token.Key()].UsedTokens != 12 {
		t.Fatal("snapshot aliases live counters")
	}
}
func TestReplyBudgetSnapshotValidation(t *testing.T) {
	for _, s := range []*ReplyBudgetSnapshot{{Version: 2}, {Version: 1, Counters: map[string]ReplyBudgetCounter{"bad": {UsedTokens: math.NaN()}}}, {Version: 1, Counters: map[string]ReplyBudgetCounter{"bad": {SpentUSD: -1}}}} {
		if _, err := WithReplyBudgetSnapshot(context.Background(), s); err == nil {
			t.Fatal("invalid snapshot accepted")
		}
	}
}

func TestRestoredCostBudgetWarnsAndStopsNextCall(t *testing.T) {
	m := NewReplyCostBudget(1, WithCostBudgetPrices(map[string]model.Price{"fixture": {Input: 100000}}))
	ctx, err := WithReplyBudgetSnapshot(context.Background(), &ReplyBudgetSnapshot{Version: 1, Counters: map[string]ReplyBudgetCounter{m.Key(): {SpentUSD: 0.8}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.OnModelCall(ctx, &ModelCallInput{ModelName: "fixture"}, func(context.Context, *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{Usage: &model.ChatUsage{InputTokens: 3}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.OnSystemPrompt(ctx, "bot", "prompt") == "prompt" {
		t.Fatal("restored budget failed to warn")
	}
	saved := SnapshotReplyBudgets(ctx)
	resumed, err := WithReplyBudgetSnapshot(context.Background(), saved)
	if err != nil {
		t.Fatal(err)
	}
	m.OnReply(resumed, ReplyInput{}, func(context.Context, ReplyInput) <-chan event.Event { return nil })
	_, err = m.OnModelCall(resumed, &ModelCallInput{}, func(context.Context, *ModelCallInput) (*model.ChatResponse, error) {
		t.Error("exhausted budget called model")
		return nil, nil
	})
	if !errors.Is(err, agenterrors.ErrBudgetExceeded) {
		t.Fatal(err)
	}
	if SnapshotReplyBudgets(context.Background()) != nil {
		t.Fatal("legacy context invented a snapshot")
	}
}

func TestConcurrentReplyCostCountersAccumulateCompletedCalls(t *testing.T) {
	m := NewReplyCostBudget(10, WithCostBudgetPrices(map[string]model.Price{"fixture": {Input: 1}}))
	ctx, err := WithReplyBudgetSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.OnModelCall(ctx, &ModelCallInput{ModelName: "fixture"}, func(context.Context, *ModelCallInput) (*model.ChatResponse, error) {
				entered <- struct{}{}
				<-release
				return &model.ChatResponse{Usage: &model.ChatUsage{InputTokens: 1000000}}, nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	<-entered
	<-entered
	close(release)
	wg.Wait()
	if spent := SnapshotReplyBudgets(ctx).Counters[m.Key()].SpentUSD; spent != 2 {
		t.Fatalf("lost concurrent observed cost: %v", spent)
	}
}
