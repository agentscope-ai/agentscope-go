package agenttest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
)

// ToolCallRecord captures a single tool invocation observed during a run.
// Input is populated from tool-call delta events when available; Name and
// Output are always populated from the event stream.
type ToolCallRecord struct {
	Name   string
	Input  map[string]any
	Output string
}

// RunResult aggregates the observable outcome of running an agent.
type RunResult struct {
	FinalOutput string
	ToolCalls   []ToolCallRecord
	Events      []event.Event
	Iterations  int
}

type runConfig struct {
	maxIters int
}

// RunOption configures a RunAgent invocation.
type RunOption func(*runConfig)

// MaxRunIterations caps the number of model-call iterations. When exceeded, the
// run context is canceled so the agent stops. Zero means no cap.
func MaxRunIterations(n int) RunOption {
	return func(c *runConfig) { c.maxIters = n }
}

// RunAgent drives a.ReplyStream, collects every event, and extracts the tool
// calls and final text output. It fails the test if the stream cannot start.
func RunAgent(t *testing.T, a *agent.UnifiedAgent, input string, opts ...RunOption) *RunResult {
	t.Helper()

	cfg := runConfig{}
	for _, o := range opts {
		o(&cfg)
	}

	ctx := context.Background()
	cancel := context.CancelFunc(func() {})
	if cfg.maxIters > 0 {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	ch, err := a.ReplyStream(ctx, input)
	if err != nil {
		t.Fatalf("agenttest: ReplyStream failed: %v", err)
	}

	result := &RunResult{}

	// Per-tool-call accumulators keyed by tool call ID.
	names := map[string]string{}
	inputDeltas := map[string]*strings.Builder{}
	outputs := map[string]*strings.Builder{}

	// Final output is the text from the last text block group.
	var curText strings.Builder

	for evt := range ch {
		result.Events = append(result.Events, evt)

		switch e := evt.(type) {
		case event.ModelCallStartEvent:
			result.Iterations++
			if cfg.maxIters > 0 && result.Iterations > cfg.maxIters {
				cancel()
			}

		case event.TextBlockStartEvent:
			curText.Reset()
		case event.TextBlockDeltaEvent:
			curText.WriteString(e.Delta)
		case event.TextBlockEndEvent:
			result.FinalOutput = curText.String()

		case event.ToolCallStartEvent:
			names[e.ToolCallID] = e.ToolCallName
			inputDeltas[e.ToolCallID] = &strings.Builder{}
		case event.ToolCallDeltaEvent:
			if b, ok := inputDeltas[e.ToolCallID]; ok {
				b.WriteString(e.Delta)
			}

		case event.ToolResultStartEvent:
			if _, ok := names[e.ToolCallID]; !ok {
				names[e.ToolCallID] = e.ToolCallName
			}
			outputs[e.ToolCallID] = &strings.Builder{}
		case event.ToolResultTextDeltaEvent:
			if b, ok := outputs[e.ToolCallID]; ok {
				b.WriteString(e.Delta)
			} else {
				sb := &strings.Builder{}
				sb.WriteString(e.Delta)
				outputs[e.ToolCallID] = sb
			}
		case event.ToolResultEndEvent:
			rec := ToolCallRecord{Name: names[e.ToolCallID]}
			if b, ok := inputDeltas[e.ToolCallID]; ok && b.Len() > 0 {
				var parsed map[string]any
				if json.Unmarshal([]byte(b.String()), &parsed) == nil {
					rec.Input = parsed
				}
			}
			if b, ok := outputs[e.ToolCallID]; ok {
				rec.Output = b.String()
			}
			result.ToolCalls = append(result.ToolCalls, rec)
		}
	}

	return result
}
