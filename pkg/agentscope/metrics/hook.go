package metrics

import (
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
)

// MetricsHook records metrics for model calls, tool executions, loop
// iterations, and active loops. It satisfies the loop.Hook interface
// structurally without importing the loop package.
type MetricsHook struct {
	modelCallTotal    Counter
	modelCallErrors   Counter
	modelCallDuration Histogram
	toolExecTotal     Counter
	toolExecErrors    Counter
	toolExecDuration  Histogram
	loopIterations    Counter
	activeLoops       Counter

	modelCallStart time.Time
	toolExecStart  time.Time
}

// NewMetricsHook creates a MetricsHook that records metrics via the given
// MetricsProvider. Pass Noop() to discard all metrics.
func NewMetricsHook(p MetricsProvider) *MetricsHook {
	return &MetricsHook{
		modelCallTotal:    p.Counter(ModelCallTotal, "Total model API calls"),
		modelCallErrors:   p.Counter(ModelCallErrors, "Total model API errors"),
		modelCallDuration: p.Histogram(ModelCallDuration, "Model call duration in seconds", []float64{0.1, 0.5, 1, 2, 5, 10, 30}),
		toolExecTotal:     p.Counter(ToolExecTotal, "Total tool executions"),
		toolExecErrors:    p.Counter(ToolExecErrors, "Total tool execution errors"),
		toolExecDuration:  p.Histogram(ToolExecDuration, "Tool execution duration in seconds", []float64{0.01, 0.1, 0.5, 1, 5, 10, 30}),
		loopIterations:    p.Counter(LoopIterations, "Total loop iterations"),
		activeLoops:       p.Counter(ActiveLoops, "Currently active loops"),
	}
}

// BeforeModelCall records the start time for the model call.
func (h *MetricsHook) BeforeModelCall(_ protocol.LoopState, _ int) {
	h.modelCallStart = time.Now()
}

// AfterModelCall increments the model call counter and records the duration.
func (h *MetricsHook) AfterModelCall(_ protocol.LoopState, _ int, err error) {
	h.modelCallTotal.Inc()
	h.modelCallDuration.Observe(time.Since(h.modelCallStart).Seconds())
	if err != nil {
		h.modelCallErrors.Inc()
	}
}

// BeforeToolExec records the start time for the tool execution.
func (h *MetricsHook) BeforeToolExec(_ protocol.LoopState, _ int, _ string) {
	h.toolExecStart = time.Now()
}

// AfterToolExec increments the tool execution counter and records the duration.
func (h *MetricsHook) AfterToolExec(_ protocol.LoopState, _ int, _ string, err error) {
	h.toolExecTotal.Inc()
	h.toolExecDuration.Observe(time.Since(h.toolExecStart).Seconds())
	if err != nil {
		h.toolExecErrors.Inc()
	}
}

// OnStateTransition counts a loop iteration each time the agent leaves the
// Reason state.
func (h *MetricsHook) OnStateTransition(from, _ protocol.LoopState, _ int) {
	if from == protocol.StateReason {
		h.loopIterations.Inc()
	}
}

// OnLoopStart increments the active loops gauge.
func (h *MetricsHook) OnLoopStart() {
	h.activeLoops.Add(1)
}

// OnLoopEnd decrements the active loops gauge.
func (h *MetricsHook) OnLoopEnd(_ error) {
	h.activeLoops.Add(-1)
}
