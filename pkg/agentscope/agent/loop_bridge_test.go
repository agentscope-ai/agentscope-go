package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/metrics"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tracing"
)

func TestUnifiedAgentRunnerLoopOptions(t *testing.T) {
	mock := &mockChatModel{
		responses: []model.ChatResponse{
			{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "Hi"}}, IsLast: true},
		},
	}
	agent := NewUnifiedAgent("bot", "prompt", mock)
	runner := NewUnifiedAgentRunner(agent)

	opts := runner.LoopOptions()
	if len(opts) == 0 {
		t.Fatal("expected non-empty options")
	}

	l := loop.New(opts...)
	result, err := l.RunSync(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if result.GetTextContent() != "Hi" {
		t.Errorf("got %q, want %q", result.GetTextContent(), "Hi")
	}
}

func TestUnifiedAgentRunnerWithToolCall(t *testing.T) {
	echoSchema := json.RawMessage(`{"type":"object","properties":{"Text":{"type":"string"}}}`)
	echoTool := tool.NewFunctionTool(
		"echo",
		"echoes input",
		echoSchema,
		func(ctx context.Context, input map[string]any) (any, error) {
			text, _ := input["Text"].(string)
			return "echoed: " + text, nil
		},
	)

	mock := &mockChatModel{
		responses: []model.ChatResponse{
			// First response: a tool call
			{Content: []message.ContentBlock{
				message.ToolCallBlock{
					Type:  "tool_use",
					ID:    "tc1",
					Name:  "echo",
					Input: `{"Text":"hello"}`,
					State: message.ToolCallPending,
				},
			}, IsLast: true},
			// Second response: final text after tool result
			{Content: []message.ContentBlock{
				message.TextBlock{Type: "text", Text: "Tool returned: echoed: hello"},
			}, IsLast: true},
		},
	}

	tk := tool.NewToolkit(echoTool)

	agent := NewUnifiedAgent("bot", "prompt", mock, WithToolkit(tk))
	runner := NewUnifiedAgentRunner(agent)

	l := loop.New(runner.LoopOptions()...)
	result, err := l.RunSync(context.Background(), "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	want := "Tool returned: echoed: hello"
	if result.GetTextContent() != want {
		t.Errorf("got %q, want %q", result.GetTextContent(), want)
	}
	if mock.callCount != 2 {
		t.Errorf("model called %d times, want 2", mock.callCount)
	}
}

func TestUnifiedAgentRunnerFailsClosedWhenToolNeedsConfirmation(t *testing.T) {
	var executions atomic.Int64
	dangerous := tool.NewFunctionTool("dangerous", "must be confirmed", nil,
		func(_ context.Context, _ map[string]any) (any, error) {
			executions.Add(1)
			return "executed", nil
		})
	mock := &mockChatModel{responses: []model.ChatResponse{
		{Content: []message.ContentBlock{message.ToolCallBlock{
			Type: "tool_use", ID: "tc-denied", Name: "dangerous", Input: `{}`,
			State: message.ToolCallPending,
		}}, IsLast: true},
		{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "not executed"}}, IsLast: true},
	}}
	a := NewUnifiedAgent("bot", "prompt", mock,
		WithToolkit(tool.NewToolkit(dangerous)),
		WithPermissionContext(permission.NewContext(permission.ModeDefault)),
	)

	result, err := loop.New(NewUnifiedAgentRunner(a).LoopOptions()...).RunSync(context.Background(), "run it")
	if err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 0 {
		t.Fatalf("dangerous tool executed %d times without confirmation", executions.Load())
	}
	if got := result.GetTextContent(); got != "not executed" {
		t.Fatalf("result = %q, want %q", got, "not executed")
	}
}

func TestLoopBridgeUsesMiddlewareRewrittenInput(t *testing.T) {
	var got string
	echo := tool.NewFunctionTool("echo", "echo", nil,
		func(_ context.Context, input map[string]any) (any, error) {
			got, _ = input["text"].(string)
			return got, nil
		})
	a := NewUnifiedAgent("bot", "prompt", &mockChatModel{},
		WithToolkit(tool.NewToolkit(echo)),
		WithMiddlewares(&rewriteActingInput{}),
		WithPermissionContext(permission.NewContext(permission.ModeBypass)),
	)
	var cfg loop.Config
	for _, opt := range NewUnifiedAgentRunner(a).LoopOptions() {
		opt(&cfg)
	}
	_, err := cfg.ToolExecutor.Execute(context.Background(), message.ToolCallBlock{
		Type: "tool_use", ID: "rewrite", Name: "echo", Input: `{"text":"original"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "sanitized" {
		t.Fatalf("tool received %q, want middleware-rewritten input", got)
	}
}

type rewriteActingInput struct{ middleware.BaseMiddleware }

func (m *rewriteActingInput) OnActing(ctx context.Context, input *middleware.ActingInput, next middleware.ActingHandler) (*tool.ToolResponse, error) {
	input.ToolCall.Input = `{"text":"sanitized"}`
	return next(ctx, input)
}

// --- In-memory metrics provider for testing ---

type atomicCounter struct{ val int64 }

func (c *atomicCounter) Inc(_ ...string)            { atomic.AddInt64(&c.val, 1) }
func (c *atomicCounter) Add(v float64, _ ...string) { atomic.AddInt64(&c.val, int64(v)) }
func (c *atomicCounter) Value() int64               { return atomic.LoadInt64(&c.val) }

type atomicHistogram struct{ count int64 }

func (h *atomicHistogram) Observe(_ float64, _ ...string) { atomic.AddInt64(&h.count, 1) }
func (h *atomicHistogram) Count() int64                   { return atomic.LoadInt64(&h.count) }

type testMetricsProvider struct {
	counters   map[string]*atomicCounter
	histograms map[string]*atomicHistogram
}

func newTestMetricsProvider() *testMetricsProvider {
	return &testMetricsProvider{
		counters:   make(map[string]*atomicCounter),
		histograms: make(map[string]*atomicHistogram),
	}
}

func (p *testMetricsProvider) Counter(name, _ string, _ ...string) metrics.Counter {
	if c, ok := p.counters[name]; ok {
		return c
	}
	c := &atomicCounter{}
	p.counters[name] = c
	return c
}

func (p *testMetricsProvider) Histogram(name, _ string, _ []float64, _ ...string) metrics.Histogram {
	if h, ok := p.histograms[name]; ok {
		return h
	}
	h := &atomicHistogram{}
	p.histograms[name] = h
	return h
}

// --- Span recorder for tracing tests ---

type spanRecord struct {
	name   string
	closed bool
}

type testTracer struct {
	spans []*spanRecord
}

func (t *testTracer) StartSpan(_ context.Context, name string) (context.Context, func()) {
	rec := &spanRecord{name: name}
	t.spans = append(t.spans, rec)
	return context.Background(), func() { rec.closed = true }
}

// --- Integration tests ---

func TestUnifiedAgentWithMetricsAndTracingHooks(t *testing.T) {
	mock := &mockChatModel{
		responses: []model.ChatResponse{
			{Content: []message.ContentBlock{
				message.ToolCallBlock{Type: "tool_use", ID: "tc1", Name: "add", Input: `{"x":1,"y":2}`, State: message.ToolCallPending},
			}, IsLast: true},
			{Content: []message.ContentBlock{
				message.TextBlock{Type: "text", Text: "3"},
			}, IsLast: true},
		},
	}

	schema := json.RawMessage(`{"type":"object","properties":{"x":{"type":"number"},"y":{"type":"number"}},"required":["x","y"]}`)
	addTool := tool.NewFunctionTool("add", "Add", schema,
		func(ctx context.Context, input map[string]any) (any, error) {
			return input["x"].(float64) + input["y"].(float64), nil
		},
	)

	mp := newTestMetricsProvider()
	metricsHook := metrics.NewMetricsHook(mp)

	tr := &testTracer{}
	tracingHook := tracing.NewTracingHook(tr)

	tk := tool.NewToolkit(addTool)
	agent := NewUnifiedAgent("bot", "prompt", mock,
		WithToolkit(tk),
		WithLoopHooks(metricsHook, tracingHook),
	)

	reply, err := agent.Reply(context.Background(), "1+2?")
	if err != nil {
		t.Fatal(err)
	}

	txt := reply.GetTextContent("\n")
	if txt == nil || *txt != "3" {
		t.Fatalf("unexpected reply: %v", txt)
	}

	// Verify metrics
	if c, ok := mp.counters[metrics.ModelCallTotal]; !ok || c.Value() != 2 {
		t.Errorf("model_call_total = %d, want 2", safeCounterValue(mp.counters[metrics.ModelCallTotal]))
	}
	if c, ok := mp.counters[metrics.ToolExecTotal]; !ok || c.Value() != 1 {
		t.Errorf("tool_exec_total = %d, want 1", safeCounterValue(mp.counters[metrics.ToolExecTotal]))
	}
	if h, ok := mp.histograms[metrics.ModelCallDuration]; !ok || h.Count() != 2 {
		t.Errorf("model_call_duration observations = %d, want 2", safeHistogramCount(mp.histograms[metrics.ModelCallDuration]))
	}

	// Verify tracing spans
	if len(tr.spans) == 0 {
		t.Fatal("expected tracing spans")
	}
	// Should have: loop.run, loop.model_call ×2, loop.tool.add ×1
	spanNames := make(map[string]int)
	for _, s := range tr.spans {
		spanNames[s.name]++
		if !s.closed {
			t.Errorf("span %q not closed", s.name)
		}
	}
	if spanNames["loop.run"] != 1 {
		t.Errorf("loop.run spans = %d, want 1", spanNames["loop.run"])
	}
	if spanNames["loop.model_call"] != 2 {
		t.Errorf("loop.model_call spans = %d, want 2", spanNames["loop.model_call"])
	}
	if spanNames["loop.tool.add"] != 1 {
		t.Errorf("loop.tool.add spans = %d, want 1", spanNames["loop.tool.add"])
	}
}

func safeCounterValue(c *atomicCounter) int64 {
	if c == nil {
		return 0
	}
	return c.Value()
}

func safeHistogramCount(h *atomicHistogram) int64 {
	if h == nil {
		return 0
	}
	return h.Count()
}

func TestModelCallerAdapterDelegation(t *testing.T) {
	mock := &mockChatModel{
		responses: []model.ChatResponse{
			{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "response"}}, IsLast: true},
		},
	}

	agent := NewUnifiedAgent("bot", "prompt", mock)
	adapter := &modelCallerAdapter{agent: agent}

	resp, err := adapter.Call(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetTextContent() != "response" {
		t.Errorf("got %q, want %q", resp.GetTextContent(), "response")
	}
	if mock.callCount != 1 {
		t.Errorf("model called %d times, want 1", mock.callCount)
	}
}
