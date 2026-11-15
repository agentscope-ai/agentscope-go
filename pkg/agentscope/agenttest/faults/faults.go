// Package faults provides fault-injection middleware for resilience chaos
// testing (HARNESS_DESIGN E4): inject model errors, tool failures, and
// latencies at configurable rates, then assert the retry/fallback/circuit
// layers behave. Deterministic via an injectable random source.
package faults

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// Config describes what to inject.
type Config struct {
	// ModelErrorRate is the probability [0,1] that a model call fails.
	ModelErrorRate float64
	// ModelError is the injected error (default: synthetic transient).
	ModelError error
	// ToolErrorRate is the probability a tool execution fails.
	ToolErrorRate float64
	// ToolLatency adds artificial latency to tool executions.
	ToolLatency time.Duration
	// Seed makes injection deterministic (0 = time-seeded).
	Seed int64
}

// Injector is the fault-injection middleware. The rng is mutex-guarded
// because OnActing runs concurrently for parallel tool batches.
type Injector struct {
	middleware.BaseMiddleware
	cfg Config
	mu  sync.Mutex
	rng *rand.Rand
}

// New creates an injector middleware.
func New(cfg Config) *Injector {
	seed := cfg.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	return &Injector{
		BaseMiddleware: middleware.BaseMiddleware{MiddlewareKey: "fault-injector"},
		cfg:            cfg,
		rng:            rand.New(rand.NewSource(seed)),
	}
}

// OnModelCall injects model failures at the configured rate.
func (f *Injector) OnModelCall(ctx context.Context, input *middleware.ModelCallInput, next middleware.ModelCallHandler) (*model.ChatResponse, error) {
	f.mu.Lock()
	roll := f.rng.Float64()
	f.mu.Unlock()
	if f.cfg.ModelErrorRate > 0 && roll < f.cfg.ModelErrorRate {
		if f.cfg.ModelError != nil {
			return nil, f.cfg.ModelError
		}
		return nil, fmt.Errorf("fault-injected model error: 503 upstream unavailable")
	}
	return next(ctx, input)
}

// OnActing injects tool failures and latency.
func (f *Injector) OnActing(ctx context.Context, input *middleware.ActingInput, next middleware.ActingHandler) (*tool.ToolResponse, error) {
	if f.cfg.ToolLatency > 0 {
		select {
		case <-time.After(f.cfg.ToolLatency):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	rollT := f.rng.Float64()
	f.mu.Unlock()
	if f.cfg.ToolErrorRate > 0 && rollT < f.cfg.ToolErrorRate {
		return nil, fmt.Errorf("fault-injected tool failure for %s", input.ToolCall.Name)
	}
	return next(ctx, input)
}

// OnReply is a pass-through (kept for interface completeness / future use).
func (f *Injector) OnReply(ctx context.Context, input middleware.ReplyInput, next middleware.ReplyHandler) <-chan event.Event {
	return next(ctx, input)
}
