package middleware

import (
	"context"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/metrics"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// DefaultReplyDurationBuckets are the histogram buckets (in seconds) used for
// agent reply duration when none are supplied.
var DefaultReplyDurationBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// MetricsMiddleware records agent observability metrics through a
// metrics.MetricsProvider. It tracks reply duration, tool-call counts, model
// token consumption, and errors. All instruments are created eagerly in the
// constructor so a scrape sees them even before the first agent run.
//
// Emitted metrics:
//   - agent_reply_duration_seconds (histogram) — labels: agent
//   - agent_tool_calls_total       (counter)   — labels: agent, tool, status
//   - agent_model_tokens_total     (counter)   — labels: agent, provider, model, type
//   - agent_errors_total           (counter)   — labels: agent, phase
type MetricsMiddleware struct {
	BaseMiddleware

	replyDuration metrics.Histogram
	toolCalls     metrics.Counter
	modelTokens   metrics.Counter
	errors        metrics.Counter
}

// NewMetricsMiddleware creates a metrics middleware backed by provider. If
// provider is nil, a no-op provider is used so the middleware is always safe to
// install.
func NewMetricsMiddleware(provider metrics.MetricsProvider) *MetricsMiddleware {
	if provider == nil {
		provider = metrics.Noop()
	}
	return &MetricsMiddleware{
		BaseMiddleware: BaseMiddleware{MiddlewareKey: "metrics"},
		replyDuration: provider.Histogram(
			"agent_reply_duration_seconds",
			"Duration of an agent reply lifecycle in seconds.",
			DefaultReplyDurationBuckets,
			"agent",
		),
		toolCalls: provider.Counter(
			"agent_tool_calls_total",
			"Total number of tool executions.",
			"agent", "tool", "status",
		),
		modelTokens: provider.Counter(
			"agent_model_tokens_total",
			"Total number of tokens consumed by model calls.",
			"agent", "provider", "model", "type",
		),
		errors: provider.Counter(
			"agent_errors_total",
			"Total number of errors encountered during agent execution.",
			"agent", "phase",
		),
	}
}

// OnReply measures the wall-clock duration of the entire reply lifecycle and
// observes it once the event stream is fully drained.
func (m *MetricsMiddleware) OnReply(ctx context.Context, input ReplyInput, next ReplyHandler) <-chan event.Event {
	start := time.Now()

	innerCh := next(ctx, input)
	outCh := make(chan event.Event, 16)

	go func() {
		defer close(outCh)
		for ev := range innerCh {
			outCh <- ev
		}
		m.replyDuration.Observe(time.Since(start).Seconds(), input.AgentName)
	}()

	return outCh
}

// OnModelCall records token usage per model call and increments the error
// counter when the call fails.
func (m *MetricsMiddleware) OnModelCall(ctx context.Context, input *ModelCallInput, next ModelCallHandler) (*model.ChatResponse, error) {
	resp, err := next(ctx, input)
	if err != nil {
		m.errors.Inc(input.AgentName, "model_call")
		return resp, err
	}

	if resp != nil && resp.Usage != nil {
		provider := input.ProviderName
		modelName := input.ModelName
		if resp.ModelName != "" {
			modelName = resp.ModelName
		}
		m.modelTokens.Add(float64(resp.Usage.InputTokens), input.AgentName, provider, modelName, "input")
		m.modelTokens.Add(float64(resp.Usage.OutputTokens), input.AgentName, provider, modelName, "output")
	}

	return resp, nil
}

// OnActing counts each tool execution, labeled by outcome, and increments the
// error counter on failure.
func (m *MetricsMiddleware) OnActing(ctx context.Context, input *ActingInput, next ActingHandler) (*tool.ToolResponse, error) {
	resp, err := next(ctx, input)

	status := "ok"
	if err != nil {
		status = "error"
		m.errors.Inc(input.AgentName, "acting")
	}
	m.toolCalls.Inc(input.AgentName, input.ToolCall.Name, status)

	return resp, err
}
