package tracing

import (
	"context"
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
)

// TracingHook is a loop.Hook implementation that creates tracing spans
// for loop lifecycle events. It satisfies the loop.Hook interface
// structurally without importing the loop package.
type TracingHook struct {
	tracer   Tracer
	ctx      context.Context
	loopEnd  func()
	modelEnd func()
	toolEnd  func()
}

// NewTracingHook creates a TracingHook backed by the given Tracer.
func NewTracingHook(t Tracer) *TracingHook {
	return &TracingHook{
		tracer: t,
		ctx:    context.Background(),
	}
}

// OnLoopStart opens a root tracing span for the loop execution.
func (h *TracingHook) OnLoopStart() {
	h.ctx, h.loopEnd = h.tracer.StartSpan(h.ctx, "loop.run")
}

// OnLoopEnd closes the root loop span.
func (h *TracingHook) OnLoopEnd(_ error) {
	if h.loopEnd != nil {
		h.loopEnd()
		h.loopEnd = nil
	}
}

// BeforeModelCall opens a child span for the model API call.
func (h *TracingHook) BeforeModelCall(_ protocol.LoopState, _ int) {
	h.ctx, h.modelEnd = h.tracer.StartSpan(h.ctx, "loop.model_call")
}

// AfterModelCall closes the model call span.
func (h *TracingHook) AfterModelCall(_ protocol.LoopState, _ int, _ error) {
	if h.modelEnd != nil {
		h.modelEnd()
		h.modelEnd = nil
	}
}

// BeforeToolExec opens a child span for the named tool execution.
func (h *TracingHook) BeforeToolExec(_ protocol.LoopState, _ int, toolName string) {
	h.ctx, h.toolEnd = h.tracer.StartSpan(h.ctx, fmt.Sprintf("loop.tool.%s", toolName))
}

// AfterToolExec closes the tool execution span.
func (h *TracingHook) AfterToolExec(_ protocol.LoopState, _ int, _ string, _ error) {
	if h.toolEnd != nil {
		h.toolEnd()
		h.toolEnd = nil
	}
}

// OnStateTransition is a no-op for the tracing hook.
func (h *TracingHook) OnStateTransition(_, _ protocol.LoopState, _ int) {}
