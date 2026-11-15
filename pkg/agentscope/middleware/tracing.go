package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tracing"
)

// TracingMiddleware creates nested spans for agent lifecycle events using a
// tracing.Tracer. It wraps OnReply (invoke_agent span), OnModelCall (chat
// span), and OnActing (execute_tool span).
//
// When the tracer implements tracing.AttributedTracer, richer span attributes
// are attached following the GenAI semantic conventions: model system, model
// name, token usage, tool name, agent name, session ID, etc.
type TracingMiddleware struct {
	BaseMiddleware
	Tracer tracing.Tracer
}

// NewTracingMiddleware creates a tracing middleware. If tracer is nil, it
// uses the globally installed tracer from tracing.TracerInstance().
func NewTracingMiddleware(tracer tracing.Tracer) *TracingMiddleware {
	if tracer == nil {
		tracer = tracing.TracerInstance()
	}
	return &TracingMiddleware{
		BaseMiddleware: BaseMiddleware{MiddlewareKey: "tracing"},
		Tracer:         tracer,
	}
}

// OnReply creates an "invoke_agent" span that wraps the entire reply lifecycle.
// GenAI attributes: gen_ai.agent.name, agentscope.session_id, agentscope.iteration.
func (m *TracingMiddleware) OnReply(ctx context.Context, input ReplyInput, next ReplyHandler) <-chan event.Event {
	spanName := fmt.Sprintf("invoke_agent %s", input.AgentName)

	attrs := []tracing.SpanAttribute{
		{Key: "agentscope.agent_name", Value: input.AgentName},
		{Key: "agentscope.message_count", Value: len(input.Messages)},
		{Key: "gen_ai.agent.name", Value: input.AgentName},
	}

	// Attach session ID from MiddleContext if available.
	if mc := GetMiddleContext(ctx); mc != nil {
		if sid, ok := mc.Get("tracing", "session_id"); ok {
			if s, ok := sid.(string); ok && s != "" {
				attrs = append(attrs, tracing.SpanAttribute{Key: "agentscope.session_id", Value: s})
			}
		}
	}

	ctx, endSpan := m.startSpan(ctx, spanName, attrs...)

	innerCh := next(ctx, input)
	outCh := make(chan event.Event, 16)

	go func() {
		defer close(outCh)
		defer endSpan()
		for ev := range innerCh {
			// Upstream-pattern late correlation (HARNESS_DESIGN A2): the
			// reply ID is born inside the reply loop, after this span was
			// opened; attach it as soon as it is observed.
			if rs, ok := ev.(event.ReplyStartEvent); ok && rs.ReplyID != "" {
				if la, ok := m.Tracer.(tracing.LateAttributer); ok {
					la.AddSpanAttr(ctx, tracing.SpanAttribute{Key: "agentscope.reply_id", Value: rs.ReplyID})
				}
			}
			outCh <- ev
		}
	}()

	return outCh
}

// OnReasoning creates an "agent_reasoning" span for each reasoning step,
// attaching the current iteration number.
func (m *TracingMiddleware) OnReasoning(ctx context.Context, input ReasoningInput, next ReasoningHandler) <-chan event.Event {
	spanName := fmt.Sprintf("agent_reasoning %s", input.AgentName)

	attrs := []tracing.SpanAttribute{
		{Key: "agentscope.agent_name", Value: input.AgentName},
		{Key: "agentscope.iteration", Value: input.Iteration},
	}

	ctx, endSpan := m.startSpan(ctx, spanName, attrs...)

	innerCh := next(ctx, input)
	outCh := make(chan event.Event, 16)

	go func() {
		defer close(outCh)
		defer endSpan()
		for ev := range innerCh {
			// Upstream-pattern late correlation (HARNESS_DESIGN A2): the
			// reply ID is born inside the reply loop, after this span was
			// opened; attach it as soon as it is observed.
			if rs, ok := ev.(event.ReplyStartEvent); ok && rs.ReplyID != "" {
				if la, ok := m.Tracer.(tracing.LateAttributer); ok {
					la.AddSpanAttr(ctx, tracing.SpanAttribute{Key: "agentscope.reply_id", Value: rs.ReplyID})
				}
			}
			outCh <- ev
		}
	}()

	return outCh
}

// OnModelCall creates a "chat" span for each model API call with GenAI
// semantic convention attributes for the request and response.
func (m *TracingMiddleware) OnModelCall(ctx context.Context, input *ModelCallInput, next ModelCallHandler) (*model.ChatResponse, error) {
	spanName := "chat"

	attrs := []tracing.SpanAttribute{
		{Key: "agentscope.agent_name", Value: input.AgentName},
		{Key: "gen_ai.tool.count", Value: len(input.Tools)},
	}

	// Upstream #2391: record what the model saw so traces can replay the
	// call. Serialized compactly and bounded to keep spans lightweight.
	if len(input.Messages) > 0 {
		attrs = append(attrs, tracing.SpanAttribute{
			Key:   "gen_ai.input.messages",
			Value: serializeMessagesForTrace(input.Messages),
		})
	}

	// Add model name from input if available.
	if input.ModelName != "" {
		attrs = append(attrs, tracing.SpanAttribute{Key: "gen_ai.request.model", Value: input.ModelName})
	}

	// Add provider name from input if available.
	if input.ProviderName != "" {
		attrs = append(attrs, tracing.SpanAttribute{Key: "gen_ai.system", Value: input.ProviderName})
	}

	// Add request-time call options if available.
	if input.MaxTokens != nil {
		attrs = append(attrs, tracing.SpanAttribute{Key: "gen_ai.request.max_tokens", Value: *input.MaxTokens})
	}
	if input.Temperature != nil {
		attrs = append(attrs, tracing.SpanAttribute{Key: "gen_ai.request.temperature", Value: *input.Temperature})
	}

	ctx, endSpan := m.startSpan(ctx, spanName, attrs...)
	defer endSpan()

	resp, err := next(ctx, input)

	// Enrich span with response attributes via a supplementary span if the
	// tracer supports attributes. We create a zero-duration child span to
	// attach post-call attributes since the parent span is about to end.
	// A nil response creates no supplementary span (established contract);
	// interrupted calls that surface a response with Error are still
	// recorded as finish reason "interrupted" (upstream #2450).
	if resp != nil {
		// Upstream #2450 records the real finish reason. Cancellation, a
		// transport/handler failure, and a provider that delivered a partial
		// reply (ChatResponse.Error, upstream #2350) are three different
		// outcomes; collapsing them into one "interrupted" label made the
		// attribute useless for telling a 429 from an abandoned consumer.
		finishReason := "stop"
		switch {
		case ctx.Err() != nil:
			finishReason = "interrupted"
		case err != nil:
			finishReason = "error"
		case resp.Error != nil:
			finishReason = "incomplete"
		case resp.StopReason != "":
			finishReason = resp.StopReason
		}
		// Marshal instead of concatenating: StopReason comes from the
		// provider, and a quote or backslash in it produced an invalid JSON
		// attribute value.
		reasonsJSON, jsonErr := json.Marshal([]string{finishReason})
		if jsonErr != nil {
			reasonsJSON = []byte(`["stop"]`)
		}
		postAttrs := []tracing.SpanAttribute{{
			Key:   "gen_ai.response.finish_reasons",
			Value: string(reasonsJSON),
		}}

		if resp.ID != "" {
			postAttrs = append(postAttrs, tracing.SpanAttribute{Key: "gen_ai.response.id", Value: resp.ID})
		}
		if resp.ModelName != "" {
			postAttrs = append(postAttrs, tracing.SpanAttribute{Key: "gen_ai.request.model", Value: resp.ModelName})
		}

		if resp.Usage != nil {
			postAttrs = append(postAttrs,
				tracing.SpanAttribute{Key: "gen_ai.usage.input_tokens", Value: resp.Usage.InputTokens},
				tracing.SpanAttribute{Key: "gen_ai.usage.output_tokens", Value: resp.Usage.OutputTokens},
			)
			if resp.Usage.CacheInputTokens > 0 {
				postAttrs = append(postAttrs, tracing.SpanAttribute{Key: "gen_ai.usage.cache_read_input_tokens", Value: resp.Usage.CacheInputTokens})
			}
		}

		if len(postAttrs) > 0 {
			_, endPost := m.startSpan(ctx, "chat.response", postAttrs...)
			endPost()
		}
	}

	return resp, err
}

// OnActing creates an "execute_tool" span for each tool execution with GenAI
// tool semantic convention attributes.
func (m *TracingMiddleware) OnActing(ctx context.Context, input *ActingInput, next ActingHandler) (*tool.ToolResponse, error) {
	spanName := fmt.Sprintf("execute_tool %s", input.ToolCall.Name)

	attrs := []tracing.SpanAttribute{
		{Key: "agentscope.tool_name", Value: input.ToolCall.Name},
		{Key: "agentscope.agent_name", Value: input.AgentName},
		{Key: "gen_ai.tool.name", Value: input.ToolCall.Name},
		{Key: "gen_ai.tool.call_id", Value: input.ToolCall.ID},
		{Key: "gen_ai.tool.input_length", Value: len(input.ToolCall.Input)},
	}

	ctx, endSpan := m.startSpan(ctx, spanName, attrs...)
	defer endSpan()

	return next(ctx, input)
}

// startSpan creates a span, preferring AttributedTracer if available.
func (m *TracingMiddleware) startSpan(ctx context.Context, name string, attrs ...tracing.SpanAttribute) (context.Context, func()) {
	if at, ok := m.Tracer.(tracing.AttributedTracer); ok {
		return at.StartSpanWithAttrs(ctx, name, attrs...)
	}
	return m.Tracer.StartSpan(ctx, name)
}

// traceMessagePreviewLen bounds each message's text in trace attributes.
const traceMessagePreviewLen = 512

// traceMessagesTotalLen bounds the whole gen_ai.input.messages attribute.
const traceMessagesTotalLen = 8192

// serializeMessagesForTrace renders messages as "role: text" lines with
// per-message previews, truncated to a total budget (upstream #2391).
// Truncation is rune-safe so span attributes stay valid UTF-8.
func serializeMessagesForTrace(msgs []*message.Msg) string {
	var sb strings.Builder
	emitted := 0
	for _, m := range msgs {
		if m == nil {
			continue
		}
		if emitted > 0 {
			sb.WriteString("\n")
		}
		emitted++
		textPtr := m.GetTextContent(" ")
		text := ""
		if textPtr != nil {
			text = *textPtr
		}
		text = truncateRunes(text, traceMessagePreviewLen)
		fmt.Fprintf(&sb, "%s: %s", m.Role, text)
		if sb.Len() > traceMessagesTotalLen {
			return truncateRunes(sb.String(), traceMessagesTotalLen) + "...(truncated)"
		}
	}
	return sb.String()
}

// truncateRunes shortens s to at most n runes, appending a marker when it
// was cut. Never splits a multi-byte rune.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "...(truncated)"
}
