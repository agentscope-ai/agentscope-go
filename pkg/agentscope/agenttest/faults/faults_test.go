package faults

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func TestInjector_ModelErrorsAtFullRate(t *testing.T) {
	inj := New(Config{ModelErrorRate: 1.0, Seed: 42})
	calls := 0
	next := func(_ context.Context, _ *middleware.ModelCallInput) (*model.ChatResponse, error) {
		calls++
		return &model.ChatResponse{}, nil
	}
	for i := 0; i < 5; i++ {
		_, err := inj.OnModelCall(context.Background(), &middleware.ModelCallInput{}, next)
		if err == nil {
			t.Fatal("expected injected error at rate 1.0")
		}
	}
	if calls != 0 {
		t.Errorf("next called %d times, want 0", calls)
	}
}

func TestInjector_ZeroRatePassesThrough(t *testing.T) {
	inj := New(Config{Seed: 1})
	next := func(_ context.Context, _ *middleware.ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{}, nil
	}
	if _, err := inj.OnModelCall(context.Background(), &middleware.ModelCallInput{}, next); err != nil {
		t.Fatalf("zero-rate must pass through: %v", err)
	}
}

func TestInjector_ToolFailuresAndLatency(t *testing.T) {
	sentinel := errors.New("custom fault")
	inj := New(Config{ToolErrorRate: 1.0, ModelError: nil, ToolLatency: 10 * time.Millisecond, Seed: 7})
	next := func(_ context.Context, _ *middleware.ActingInput) (*tool.ToolResponse, error) {
		return tool.NewTextResponse("ok"), nil
	}
	start := time.Now()
	_, err := inj.OnActing(context.Background(),
		&middleware.ActingInput{ToolCall: message.ToolCallBlock{Name: "t"}}, next)
	if err == nil {
		t.Fatal("expected injected tool error")
	}
	if time.Since(start) < 10*time.Millisecond {
		t.Error("latency not applied")
	}
	_ = sentinel
}

// TestInjector_ChaosMix exercises a mixed-rate run end-to-end over many
// calls to catch probability-path panics.
func TestInjector_ChaosMix(t *testing.T) {
	inj := New(Config{ModelErrorRate: 0.3, ToolErrorRate: 0.3, ToolLatency: time.Millisecond, Seed: 9})
	mNext := func(_ context.Context, _ *middleware.ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{}, nil
	}
	tNext := func(_ context.Context, _ *middleware.ActingInput) (*tool.ToolResponse, error) {
		return tool.NewTextResponse("ok"), nil
	}
	var mErrs, tErrs int
	for i := 0; i < 200; i++ {
		if _, err := inj.OnModelCall(context.Background(), &middleware.ModelCallInput{}, mNext); err != nil {
			mErrs++
		}
		if _, err := inj.OnActing(context.Background(), &middleware.ActingInput{ToolCall: message.ToolCallBlock{Name: "t"}}, tNext); err != nil {
			tErrs++
		}
	}
	if mErrs == 0 || mErrs == 200 {
		t.Errorf("model error count %d not mixed as expected", mErrs)
	}
	if tErrs == 0 || tErrs == 200 {
		t.Errorf("tool error count %d not mixed as expected", tErrs)
	}
}
