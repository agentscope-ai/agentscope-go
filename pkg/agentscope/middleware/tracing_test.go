package middleware

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tracing"
)

// recordingTracer records span names and their nesting via context propagation.
type recordingTracer struct {
	mu    sync.Mutex
	spans []spanRecord
}

type spanRecord struct {
	name   string
	parent string // parent span name if nested, empty for root
	ended  bool
	attrs  []tracing.SpanAttribute // only populated by attributedRecordingTracer
}

type spanCtxKey struct{}

func (t *recordingTracer) StartSpan(ctx context.Context, name string) (context.Context, func()) {
	t.mu.Lock()

	parent := ""
	if p, ok := ctx.Value(spanCtxKey{}).(string); ok {
		parent = p
	}
	idx := len(t.spans)
	t.spans = append(t.spans, spanRecord{name: name, parent: parent})
	t.mu.Unlock()

	ctx = context.WithValue(ctx, spanCtxKey{}, name)
	return ctx, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.spans[idx].ended = true
	}
}

// attributedRecordingTracer extends recordingTracer to capture span attributes.
// It implements tracing.AttributedTracer.
type attributedRecordingTracer struct {
	mu    sync.Mutex
	spans []spanRecord
}

func (t *attributedRecordingTracer) StartSpan(ctx context.Context, name string) (context.Context, func()) {
	return t.StartSpanWithAttrs(ctx, name)
}

func (t *attributedRecordingTracer) StartSpanWithAttrs(ctx context.Context, name string, attrs ...tracing.SpanAttribute) (context.Context, func()) {
	t.mu.Lock()

	parent := ""
	if p, ok := ctx.Value(spanCtxKey{}).(string); ok {
		parent = p
	}
	idx := len(t.spans)
	t.spans = append(t.spans, spanRecord{name: name, parent: parent, attrs: attrs})
	t.mu.Unlock()

	ctx = context.WithValue(ctx, spanCtxKey{}, name)
	return ctx, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.spans[idx].ended = true
	}
}

// findAttr returns the value of an attribute by key from a span record.
func findAttr(s spanRecord, key string) (any, bool) {
	for _, a := range s.attrs {
		if a.Key == key {
			return a.Value, true
		}
	}
	return nil, false
}

func TestTracingMiddleware_ReplySpan(t *testing.T) {
	tracer := &recordingTracer{}
	mw := NewTracingMiddleware(tracer)

	next := func(ctx context.Context, input ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, 1)
		ch <- event.NewReplyEndEvent("s1", "r1")
		close(ch)
		return ch
	}

	ch := mw.OnReply(context.Background(), ReplyInput{AgentName: "TestAgent"}, next)
	for range ch {
	}

	if len(tracer.spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(tracer.spans))
	}
	s := tracer.spans[0]
	if !strings.Contains(s.name, "invoke_agent TestAgent") {
		t.Errorf("expected invoke_agent span, got %q", s.name)
	}
	if !s.ended {
		t.Error("span should be ended after reply completes")
	}
}

func TestTracingMiddleware_ModelCallSpan(t *testing.T) {
	tracer := &recordingTracer{}
	mw := NewTracingMiddleware(tracer)

	core := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "hi"}},
		}, nil
	}

	_, err := mw.OnModelCall(context.Background(), &ModelCallInput{AgentName: "test"}, core)
	if err != nil {
		t.Fatal(err)
	}

	// Since upstream #2450 every model call with a response also records a
	// supplementary chat.response span carrying gen_ai.response.finish_reasons.
	if len(tracer.spans) != 2 {
		t.Fatalf("expected 2 spans (chat + chat.response), got %d", len(tracer.spans))
	}
	if tracer.spans[0].name != "chat" {
		t.Errorf("expected 'chat' span, got %q", tracer.spans[0].name)
	}
	if !tracer.spans[0].ended {
		t.Error("chat span should be ended")
	}
	if tracer.spans[1].name != "chat.response" {
		t.Errorf("expected 'chat.response' span, got %q", tracer.spans[1].name)
	}
}

func TestTracingMiddleware_ActingSpan(t *testing.T) {
	tracer := &recordingTracer{}
	mw := NewTracingMiddleware(tracer)

	core := func(_ context.Context, _ *ActingInput) (*tool.ToolResponse, error) {
		return tool.NewTextResponse("done"), nil
	}

	_, err := mw.OnActing(context.Background(), &ActingInput{
		AgentName: "test",
		ToolCall:  message.ToolCallBlock{Name: "search"},
	}, core)
	if err != nil {
		t.Fatal(err)
	}

	if len(tracer.spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(tracer.spans))
	}
	if tracer.spans[0].name != "execute_tool search" {
		t.Errorf("expected 'execute_tool search', got %q", tracer.spans[0].name)
	}
}

func TestTracingMiddleware_NestedSpans(t *testing.T) {
	tracer := &recordingTracer{}
	mw := NewTracingMiddleware(tracer)

	// Simulate: OnReply wraps OnModelCall which wraps OnActing
	// Build chains
	modelCore := func(ctx context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		// Simulate a tool call within the model call
		actingCore := func(ctx context.Context, _ *ActingInput) (*tool.ToolResponse, error) {
			return tool.NewTextResponse("tool result"), nil
		}
		_, _ = mw.OnActing(ctx, &ActingInput{ToolCall: message.ToolCallBlock{Name: "calc"}}, actingCore)
		return &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "hi"}},
		}, nil
	}

	replyCore := func(ctx context.Context, _ ReplyInput) <-chan event.Event {
		_, _ = mw.OnModelCall(ctx, &ModelCallInput{}, modelCore)
		ch := make(chan event.Event)
		close(ch)
		return ch
	}

	ch := mw.OnReply(context.Background(), ReplyInput{AgentName: "Agent"}, replyCore)
	for range ch {
	}

	if len(tracer.spans) != 4 {
		t.Fatalf("expected 4 nested spans (invoke_agent/chat/execute_tool/chat.response), got %d", len(tracer.spans))
	}

	// Verify nesting: invoke_agent -> chat -> execute_tool
	if tracer.spans[0].name != "invoke_agent Agent" {
		t.Errorf("span 0: %q", tracer.spans[0].name)
	}
	if tracer.spans[1].parent != "invoke_agent Agent" {
		t.Errorf("chat span should be child of invoke_agent, parent=%q", tracer.spans[1].parent)
	}
	if tracer.spans[2].parent != "chat" {
		t.Errorf("execute_tool should be child of chat, parent=%q", tracer.spans[2].parent)
	}

	for i, s := range tracer.spans {
		if !s.ended {
			t.Errorf("span %d (%s) not ended", i, s.name)
		}
	}
}

func TestTracingMiddleware_Key(t *testing.T) {
	mw := NewTracingMiddleware(nil)
	if mw.Key() != "tracing" {
		t.Errorf("expected key 'tracing', got %s", mw.Key())
	}
}

func TestTracingMiddleware_NilTracer_UsesGlobal(t *testing.T) {
	mw := NewTracingMiddleware(nil)
	if mw.Tracer == nil {
		t.Error("should fall back to global tracer instance")
	}
}

func TestTracingMiddleware_EventsPassthrough(t *testing.T) {
	tracer := &recordingTracer{}
	mw := NewTracingMiddleware(tracer)

	events := []event.Event{
		event.NewReplyStartEvent("s1", "r1", "agent", "assistant"),
		event.NewTextBlockDeltaEvent("r1", "b1", "hello"),
		event.NewReplyEndEvent("s1", "r1"),
	}

	next := func(_ context.Context, _ ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, len(events))
		for _, ev := range events {
			ch <- ev
		}
		close(ch)
		return ch
	}

	ch := mw.OnReply(context.Background(), ReplyInput{AgentName: "test"}, next)
	var count int
	for range ch {
		count++
	}
	if count != 3 {
		t.Errorf("expected 3 pass-through events, got %d", count)
	}
}

// ==================== GenAI Semantic Convention Attributes ====================

func TestTracingMiddleware_OnReply_GenAIAttributes(t *testing.T) {
	tracer := &attributedRecordingTracer{}
	mw := NewTracingMiddleware(tracer)

	// Set up MiddleContext with session_id.
	mc := MiddleContext{}
	mc.Set("tracing", "session_id", "sess-42")
	ctx := WithMiddleContext(context.Background(), mc)

	next := func(ctx context.Context, _ ReplyInput) <-chan event.Event {
		ch := make(chan event.Event)
		close(ch)
		return ch
	}

	ch := mw.OnReply(ctx, ReplyInput{AgentName: "TestAgent"}, next)
	for range ch {
	}

	if len(tracer.spans) < 1 {
		t.Fatalf("expected at least 1 span, got %d", len(tracer.spans))
	}
	s := tracer.spans[0]

	if v, ok := findAttr(s, "gen_ai.agent.name"); !ok || v != "TestAgent" {
		t.Errorf("gen_ai.agent.name = %v, want TestAgent", v)
	}
	if v, ok := findAttr(s, "agentscope.session_id"); !ok || v != "sess-42" {
		t.Errorf("agentscope.session_id = %v, want sess-42", v)
	}
}

func TestTracingMiddleware_OnReply_NoSessionID(t *testing.T) {
	tracer := &attributedRecordingTracer{}
	mw := NewTracingMiddleware(tracer)

	next := func(ctx context.Context, _ ReplyInput) <-chan event.Event {
		ch := make(chan event.Event)
		close(ch)
		return ch
	}

	// No MiddleContext at all.
	ch := mw.OnReply(context.Background(), ReplyInput{AgentName: "Agent"}, next)
	for range ch {
	}

	s := tracer.spans[0]
	if _, ok := findAttr(s, "agentscope.session_id"); ok {
		t.Error("should not have session_id when not in MiddleContext")
	}
}

func TestTracingMiddleware_OnModelCall_GenAIAttributes(t *testing.T) {
	tracer := &attributedRecordingTracer{}
	mw := NewTracingMiddleware(tracer)

	temp := 0.7
	maxTok := 1024

	core := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			ID:        "resp-123",
			ModelName: "gpt-4.1",
			Content:   []message.ContentBlock{message.TextBlock{Type: "text", Text: "hi"}},
			Usage: &model.ChatUsage{
				InputTokens:      100,
				OutputTokens:     50,
				CacheInputTokens: 25,
			},
		}, nil
	}

	_, err := mw.OnModelCall(context.Background(), &ModelCallInput{
		AgentName:    "test",
		ModelName:    "gpt-4.1",
		ProviderName: "openai",
		Temperature:  &temp,
		MaxTokens:    &maxTok,
		Tools: []model.ToolSchema{
			{Type: "function", Function: model.ToolFunction{Name: "tool1"}},
			{Type: "function", Function: model.ToolFunction{Name: "tool2"}},
		},
	}, core)
	if err != nil {
		t.Fatal(err)
	}

	// Should have 2 spans: "chat" and "chat.response"
	if len(tracer.spans) < 2 {
		t.Fatalf("expected at least 2 spans (chat + chat.response), got %d", len(tracer.spans))
	}

	chat := tracer.spans[0]
	if v, ok := findAttr(chat, "gen_ai.system"); !ok || v != "openai" {
		t.Errorf("gen_ai.system = %v, want openai", v)
	}
	if v, ok := findAttr(chat, "gen_ai.request.model"); !ok || v != "gpt-4.1" {
		t.Errorf("gen_ai.request.model = %v, want gpt-4.1", v)
	}
	if v, ok := findAttr(chat, "gen_ai.tool.count"); !ok || v != 2 {
		t.Errorf("gen_ai.tool.count = %v, want 2", v)
	}
	if v, ok := findAttr(chat, "gen_ai.request.temperature"); !ok {
		t.Error("gen_ai.request.temperature missing")
	} else if f, ok := v.(float64); !ok || f != 0.7 {
		t.Errorf("gen_ai.request.temperature = %v, want 0.7", v)
	}
	if v, ok := findAttr(chat, "gen_ai.request.max_tokens"); !ok || v != 1024 {
		t.Errorf("gen_ai.request.max_tokens = %v, want 1024", v)
	}

	resp := tracer.spans[1]
	if v, ok := findAttr(resp, "gen_ai.response.id"); !ok || v != "resp-123" {
		t.Errorf("gen_ai.response.id = %v, want resp-123", v)
	}
	if v, ok := findAttr(resp, "gen_ai.usage.input_tokens"); !ok || v != 100 {
		t.Errorf("gen_ai.usage.input_tokens = %v, want 100", v)
	}
	if v, ok := findAttr(resp, "gen_ai.usage.output_tokens"); !ok || v != 50 {
		t.Errorf("gen_ai.usage.output_tokens = %v, want 50", v)
	}
	if v, ok := findAttr(resp, "gen_ai.usage.cache_read_input_tokens"); !ok || v != 25 {
		t.Errorf("gen_ai.usage.cache_read_input_tokens = %v, want 25", v)
	}
}

func TestTracingMiddleware_OnModelCall_NoCacheTokens_NoAttribute(t *testing.T) {
	tracer := &attributedRecordingTracer{}
	mw := NewTracingMiddleware(tracer)

	core := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "hi"}},
			Usage: &model.ChatUsage{
				InputTokens:  100,
				OutputTokens: 50,
				// CacheInputTokens is 0 (default)
			},
		}, nil
	}

	_, _ = mw.OnModelCall(context.Background(), &ModelCallInput{}, core)

	// Find the response span
	if len(tracer.spans) < 2 {
		t.Fatalf("expected 2 spans, got %d", len(tracer.spans))
	}
	resp := tracer.spans[1]
	if _, ok := findAttr(resp, "gen_ai.usage.cache_read_input_tokens"); ok {
		t.Error("should not include cache_read_input_tokens when value is 0")
	}
}

func TestTracingMiddleware_OnModelCall_NilResponse_NoResponseSpan(t *testing.T) {
	tracer := &attributedRecordingTracer{}
	mw := NewTracingMiddleware(tracer)

	core := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return nil, context.Canceled
	}

	_, _ = mw.OnModelCall(context.Background(), &ModelCallInput{}, core)

	// Only the "chat" span should exist (no response span).
	if len(tracer.spans) != 1 {
		t.Errorf("expected 1 span for nil response, got %d", len(tracer.spans))
	}
}

func TestTracingMiddleware_OnActing_GenAIAttributes(t *testing.T) {
	tracer := &attributedRecordingTracer{}
	mw := NewTracingMiddleware(tracer)

	core := func(_ context.Context, _ *ActingInput) (*tool.ToolResponse, error) {
		return tool.NewTextResponse("done"), nil
	}

	_, err := mw.OnActing(context.Background(), &ActingInput{
		AgentName: "test",
		ToolCall: message.ToolCallBlock{
			ID:    "call-456",
			Name:  "search",
			Input: `{"query": "hello world"}`,
		},
	}, core)
	if err != nil {
		t.Fatal(err)
	}

	if len(tracer.spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(tracer.spans))
	}
	s := tracer.spans[0]

	if v, ok := findAttr(s, "gen_ai.tool.name"); !ok || v != "search" {
		t.Errorf("gen_ai.tool.name = %v, want search", v)
	}
	if v, ok := findAttr(s, "gen_ai.tool.call_id"); !ok || v != "call-456" {
		t.Errorf("gen_ai.tool.call_id = %v, want call-456", v)
	}
	if v, ok := findAttr(s, "gen_ai.tool.input_length"); !ok || v != 24 {
		t.Errorf("gen_ai.tool.input_length = %v, want 24", v)
	}
}

func TestTracingMiddleware_OnReasoning_Iteration(t *testing.T) {
	tracer := &attributedRecordingTracer{}
	mw := NewTracingMiddleware(tracer)

	next := func(ctx context.Context, _ ReasoningInput) <-chan event.Event {
		ch := make(chan event.Event)
		close(ch)
		return ch
	}

	ch := mw.OnReasoning(context.Background(), ReasoningInput{
		AgentName: "ReAct",
		Iteration: 3,
	}, next)
	for range ch {
	}

	if len(tracer.spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(tracer.spans))
	}
	s := tracer.spans[0]

	if !strings.Contains(s.name, "agent_reasoning ReAct") {
		t.Errorf("expected reasoning span name, got %q", s.name)
	}
	if v, ok := findAttr(s, "agentscope.iteration"); !ok || v != 3 {
		t.Errorf("agentscope.iteration = %v, want 3", v)
	}
}

func TestTracingMiddleware_ChatSpanIncludesInputMessages(t *testing.T) {
	// Upstream #2391: chat spans must carry the input messages observed at
	// the tracing middleware so traces can replay what the model saw.
	tracer := &attributedRecordingTracer{}
	mw := NewTracingMiddleware(tracer)

	core := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "2"}},
		}, nil
	}

	msgs := []*message.Msg{
		message.SystemMsg("sys", "be helpful"),
		message.UserMsg("user", "what is 1+1"),
	}
	_, err := mw.OnModelCall(context.Background(), &ModelCallInput{
		AgentName: "test",
		Messages:  msgs,
	}, core)
	if err != nil {
		t.Fatal(err)
	}

	if len(tracer.spans) != 2 {
		t.Fatalf("expected 2 spans (chat + chat.response), got %d", len(tracer.spans))
	}
	v, ok := findAttr(tracer.spans[0], "gen_ai.input.messages")
	if !ok {
		t.Fatal("chat span missing gen_ai.input.messages attribute")
	}
	found, _ := v.(string)
	if found == "" {
		t.Fatal("gen_ai.input.messages attribute is empty")
	}
	if !strings.Contains(found, "what is 1+1") {
		t.Errorf("input messages attribute should contain the user text, got %q", found)
	}
	if !strings.Contains(found, "user") {
		t.Errorf("input messages attribute should carry roles, got %q", found)
	}
}

type lateAttributingTracer struct {
	attributedRecordingTracer
	late []tracing.SpanAttribute
}

func (t *lateAttributingTracer) AddSpanAttr(_ context.Context, attr tracing.SpanAttribute) {
	t.late = append(t.late, attr)
}

func TestTracingMiddleware_ReplyIDLateAttribute(t *testing.T) {
	// HARNESS_DESIGN A2: the invoke_agent span is opened before the reply
	// loop mints the reply ID; the ID must be attached late, when the
	// ReplyStartEvent flows through.
	tracer := &lateAttributingTracer{}
	mw := NewTracingMiddleware(tracer)

	core := func(_ context.Context, _ ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, 2)
		ch <- event.NewReplyStartEvent("s1", "reply-late-1", "agent", message.RoleAssistant)
		ch <- event.NewReplyEndEvent("s1", "reply-late-1")
		close(ch)
		return ch
	}

	out := mw.OnReply(context.Background(), ReplyInput{AgentName: "agent"}, core)
	for range out {
	}

	if len(tracer.late) != 1 {
		t.Fatalf("expected 1 late attribute, got %d", len(tracer.late))
	}
	if tracer.late[0].Key != "agentscope.reply_id" || tracer.late[0].Value != "reply-late-1" {
		t.Errorf("late attr = %+v", tracer.late[0])
	}
}

func TestTracingMiddleware_NoLateAttrWithoutSupport(t *testing.T) {
	// A plain recording tracer (no LateAttributer) must not break.
	tracer := &recordingTracer{}
	mw := NewTracingMiddleware(tracer)
	core := func(_ context.Context, _ ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, 2)
		ch <- event.NewReplyStartEvent("s1", "reply-x", "agent", message.RoleAssistant)
		ch <- event.NewReplyEndEvent("s1", "reply-x")
		close(ch)
		return ch
	}
	out := mw.OnReply(context.Background(), ReplyInput{AgentName: "agent"}, core)
	n := 0
	for range out {
		n++
	}
	if n != 2 {
		t.Errorf("events = %d, want 2", n)
	}
}
