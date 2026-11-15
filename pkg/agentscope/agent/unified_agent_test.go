package agent

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func TestUnifiedAgentReplyNativeToolCalling(t *testing.T) {
	apiKey := os.Getenv("DASHSCOPE_API_KEY")
	if apiKey == "" {
		t.Skip("DASHSCOPE_API_KEY not set")
	}

	m, err := model.NewDashScopeChatModel(model.DashScopeConfig{
		APIKey: apiKey,
		Model:  "qwen-plus",
	})
	if err != nil {
		t.Fatal(err)
	}

	schema := json.RawMessage(`{"type":"object","properties":{"location":{"type":"string","description":"City name"}},"required":["location"]}`)
	weatherTool := tool.NewFunctionTool("get_weather", "Get current weather for a city", schema,
		func(ctx context.Context, input map[string]any) (any, error) {
			loc, _ := input["location"].(string)
			return map[string]any{"location": loc, "temperature": "22°C", "condition": "sunny"}, nil
		},
	)

	tk := tool.NewToolkit(weatherTool)
	agent := NewUnifiedAgent("test-agent", "You are a helpful assistant.", m,
		WithToolkit(tk),
		WithReactConfig(ReactConfig{MaxIters: 5}),
	)

	reply, err := agent.Reply(context.Background(), "What's the weather in Beijing?")
	if err != nil {
		t.Fatal(err)
	}

	txt := reply.GetTextContent("\n")
	if txt == nil || *txt == "" {
		t.Fatal("expected non-empty text response")
	}
	t.Logf("Agent reply: %s", *txt)

	// The response should mention temperature or weather info
	if !containsAny(*txt, "22", "sunny", "Beijing", "weather") {
		t.Logf("Warning: response may not contain expected weather info: %s", *txt)
	}
}

func TestUnifiedAgentReplyStream(t *testing.T) {
	apiKey := os.Getenv("DASHSCOPE_API_KEY")
	if apiKey == "" {
		t.Skip("DASHSCOPE_API_KEY not set")
	}

	m, err := model.NewDashScopeChatModel(model.DashScopeConfig{
		APIKey: apiKey,
		Model:  "qwen-plus",
	})
	if err != nil {
		t.Fatal(err)
	}

	agent := NewUnifiedAgent("test-agent", "You are a helpful assistant. Be brief.", m)

	ch, err := agent.ReplyStream(context.Background(), "Say hello in one sentence.")
	if err != nil {
		t.Fatal(err)
	}

	var (
		gotReplyStart bool
		gotReplyEnd   bool
		gotTextDelta  bool
		gotModelCall  bool
	)
	for evt := range ch {
		switch evt.(type) {
		case event.ReplyStartEvent:
			gotReplyStart = true
		case event.ReplyEndEvent:
			gotReplyEnd = true
		case event.TextBlockDeltaEvent:
			gotTextDelta = true
		case event.ModelCallStartEvent:
			gotModelCall = true
		}
	}

	if !gotReplyStart {
		t.Error("missing ReplyStartEvent")
	}
	if !gotReplyEnd {
		t.Error("missing ReplyEndEvent")
	}
	if !gotTextDelta {
		t.Error("missing TextBlockDeltaEvent")
	}
	if !gotModelCall {
		t.Error("missing ModelCallStartEvent")
	}
}

func TestUnifiedAgentWithMockModel(t *testing.T) {
	mock := &mockChatModel{
		responses: []model.ChatResponse{
			{
				Content: []message.ContentBlock{
					message.ToolCallBlock{
						Type:  "tool_call",
						ID:    "call_1",
						Name:  "add",
						Input: `{"x":1,"y":2}`,
						State: message.ToolCallPending,
					},
				},
				IsLast: true,
			},
			{
				Content: []message.ContentBlock{
					message.TextBlock{Type: "text", ID: "text_1", Text: "The result is 3."},
				},
				IsLast: true,
			},
		},
	}

	schema := json.RawMessage(`{"type":"object","properties":{"x":{"type":"number"},"y":{"type":"number"}},"required":["x","y"]}`)
	addTool := tool.NewFunctionTool("add", "Add two numbers", schema,
		func(ctx context.Context, input map[string]any) (any, error) {
			x := input["x"].(float64)
			y := input["y"].(float64)
			return x + y, nil
		},
	)

	tk := tool.NewToolkit(addTool)
	agent := NewUnifiedAgent("test", "You are a calculator.", mock, WithToolkit(tk))

	reply, err := agent.Reply(context.Background(), "What is 1+2?")
	if err != nil {
		t.Fatal(err)
	}

	txt := reply.GetTextContent("\n")
	if txt == nil || *txt != "The result is 3." {
		t.Fatalf("unexpected reply: %v", txt)
	}

	if mock.callCount != 2 {
		t.Fatalf("expected 2 model calls, got %d", mock.callCount)
	}
}

type mockChatModel struct {
	responses []model.ChatResponse
	callCount int
}

func (m *mockChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	if m.callCount >= len(m.responses) {
		return &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}},
			IsLast:  true,
		}, nil
	}
	resp := m.responses[m.callCount]
	m.callCount++
	return &resp, nil
}

func (m *mockChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (m *mockChatModel) CountTokens(msgs []*message.Msg, tools []model.ToolSchema) int {
	return 100
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if len(sub) > 0 && contains(s, sub) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// --- Middleware integration tests ---

// testLoggingMiddleware counts how many times each hook is invoked.
type testLoggingMiddleware struct {
	middleware.BaseMiddleware
	modelCallCount int64
	actingCount    int64
	promptSuffix   string
}

func (m *testLoggingMiddleware) OnModelCall(ctx context.Context, input *middleware.ModelCallInput, next middleware.ModelCallHandler) (*model.ChatResponse, error) {
	atomic.AddInt64(&m.modelCallCount, 1)
	return next(ctx, input)
}

func (m *testLoggingMiddleware) OnActing(ctx context.Context, input *middleware.ActingInput, next middleware.ActingHandler) (*tool.ToolResponse, error) {
	atomic.AddInt64(&m.actingCount, 1)
	return next(ctx, input)
}

func (m *testLoggingMiddleware) OnSystemPrompt(_ context.Context, _ string, prompt string) string {
	return prompt + m.promptSuffix
}

func TestUnifiedAgentWithMiddleware(t *testing.T) {
	mock := &mockChatModel{
		responses: []model.ChatResponse{
			{
				Content: []message.ContentBlock{
					message.ToolCallBlock{
						Type:  "tool_call",
						ID:    "call_1",
						Name:  "greet",
						Input: `{"name":"world"}`,
						State: message.ToolCallPending,
					},
				},
				IsLast: true,
			},
			{
				Content: []message.ContentBlock{
					message.TextBlock{Type: "text", ID: "txt_1", Text: "Hello world!"},
				},
				IsLast: true,
			},
		},
	}

	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
	greetTool := tool.NewFunctionTool("greet", "Greet someone", schema,
		func(ctx context.Context, input map[string]any) (any, error) {
			return "Hi " + input["name"].(string), nil
		},
	)

	mw := &testLoggingMiddleware{
		BaseMiddleware: middleware.BaseMiddleware{MiddlewareKey: "logger"},
		promptSuffix:   "\nAlways be polite.",
	}

	tk := tool.NewToolkit(greetTool)
	agent := NewUnifiedAgent("test", "You are helpful.", mock,
		WithToolkit(tk),
		WithMiddlewares(mw),
	)

	reply, err := agent.Reply(context.Background(), "Say hello")
	if err != nil {
		t.Fatal(err)
	}

	txt := reply.GetTextContent("\n")
	if txt == nil || *txt != "Hello world!" {
		t.Fatalf("unexpected reply: %v", txt)
	}

	// OnModelCall should have been called twice (one for tool call, one for final)
	if got := atomic.LoadInt64(&mw.modelCallCount); got != 2 {
		t.Errorf("OnModelCall called %d times, want 2", got)
	}

	// OnActing should have been called once (for the greet tool)
	if got := atomic.LoadInt64(&mw.actingCount); got != 1 {
		t.Errorf("OnActing called %d times, want 1", got)
	}

	// Verify system prompt was modified — check that the mock received messages with modified prompt.
	// The mock doesn't store messages, but we can verify by checking the agent worked correctly.
}

func TestUnifiedAgentMiddleware_SystemPromptPipeline(t *testing.T) {
	// Verifies that OnSystemPrompt transforms the prompt seen by the model.
	var capturedMsgs []*message.Msg
	mock := &captureMockModel{
		response: model.ChatResponse{
			Content: []message.ContentBlock{
				message.TextBlock{Type: "text", ID: "t1", Text: "OK"},
			},
			IsLast: true,
		},
		captured: &capturedMsgs,
	}

	mw := &testLoggingMiddleware{
		BaseMiddleware: middleware.BaseMiddleware{MiddlewareKey: "prompt"},
		promptSuffix:   " [INJECTED]",
	}

	agent := NewUnifiedAgent("bot", "Base prompt.", mock, WithMiddlewares(mw))
	_, err := agent.Reply(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}

	// The first message should be the system prompt with the suffix.
	if len(capturedMsgs) == 0 {
		t.Fatal("no messages captured")
	}
	sysTxt := capturedMsgs[0].GetTextContent("\n")
	if sysTxt == nil {
		t.Fatal("system message has no text")
	}
	if *sysTxt != "Base prompt. [INJECTED]" {
		t.Errorf("system prompt = %q, want %q", *sysTxt, "Base prompt. [INJECTED]")
	}
}

type captureMockModel struct {
	response model.ChatResponse
	captured *[]*message.Msg
}

func (m *captureMockModel) Chat(_ context.Context, msgs []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	*m.captured = msgs
	return &m.response, nil
}

func (m *captureMockModel) ChatStream(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (m *captureMockModel) CountTokens(_ []*message.Msg, _ []model.ToolSchema) int { return 0 }

// --- Loop hook integration tests ---

type hookRecorder struct {
	calls []string
}

func (h *hookRecorder) BeforeModelCall(_ protocol.LoopState, _ int) {
	h.calls = append(h.calls, "before_model")
}

func (h *hookRecorder) AfterModelCall(_ protocol.LoopState, _ int, _ error) {
	h.calls = append(h.calls, "after_model")
}

func (h *hookRecorder) BeforeToolExec(_ protocol.LoopState, _ int, name string) {
	h.calls = append(h.calls, "before_tool:"+name)
}

func (h *hookRecorder) AfterToolExec(_ protocol.LoopState, _ int, name string, _ error) {
	h.calls = append(h.calls, "after_tool:"+name)
}

func (h *hookRecorder) OnStateTransition(from, to protocol.LoopState, _ int) {
	h.calls = append(h.calls, "transition:"+from.String()+"->"+to.String())
}

func (h *hookRecorder) OnLoopStart() {
	h.calls = append(h.calls, "loop_start")
}

func (h *hookRecorder) OnLoopEnd(_ error) {
	h.calls = append(h.calls, "loop_end")
}

func TestUnifiedAgentLoopHooks_SimpleChat(t *testing.T) {
	mock := &mockChatModel{
		responses: []model.ChatResponse{
			{
				Content: []message.ContentBlock{
					message.TextBlock{Type: "text", Text: "Hello!"},
				},
				IsLast: true,
			},
		},
	}

	rec := &hookRecorder{}
	agent := NewUnifiedAgent("bot", "prompt", mock, WithLoopHooks(rec))

	_, err := agent.Reply(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}

	expected := []string{
		"loop_start",
		"before_model",
		"after_model",
		"transition:reason->inspect",
		"transition:inspect->exit",
		"loop_end",
	}
	if len(rec.calls) != len(expected) {
		t.Fatalf("got %d calls %v, want %d %v", len(rec.calls), rec.calls, len(expected), expected)
	}
	for i, want := range expected {
		if rec.calls[i] != want {
			t.Errorf("call[%d] = %q, want %q", i, rec.calls[i], want)
		}
	}
}

func TestUnifiedAgentLoopHooks_WithToolCall(t *testing.T) {
	mock := &mockChatModel{
		responses: []model.ChatResponse{
			{
				Content: []message.ContentBlock{
					message.ToolCallBlock{Type: "tool_call", ID: "c1", Name: "add", Input: `{"x":1,"y":2}`, State: message.ToolCallPending},
				},
				IsLast: true,
			},
			{
				Content: []message.ContentBlock{
					message.TextBlock{Type: "text", Text: "3"},
				},
				IsLast: true,
			},
		},
	}

	schema := json.RawMessage(`{"type":"object","properties":{"x":{"type":"number"},"y":{"type":"number"}},"required":["x","y"]}`)
	addTool := tool.NewFunctionTool("add", "Add", schema,
		func(ctx context.Context, input map[string]any) (any, error) {
			return input["x"].(float64) + input["y"].(float64), nil
		},
	)

	rec := &hookRecorder{}
	tk := tool.NewToolkit(addTool)
	agent := NewUnifiedAgent("bot", "prompt", mock, WithToolkit(tk), WithLoopHooks(rec))

	_, err := agent.Reply(context.Background(), "1+2?")
	if err != nil {
		t.Fatal(err)
	}

	// Verify key hook calls are present
	hasLoopStart := false
	hasLoopEnd := false
	modelCallCount := 0
	toolCallCount := 0
	for _, c := range rec.calls {
		switch {
		case c == "loop_start":
			hasLoopStart = true
		case c == "loop_end":
			hasLoopEnd = true
		case c == "before_model":
			modelCallCount++
		case c == "before_tool:add":
			toolCallCount++
		}
	}

	if !hasLoopStart {
		t.Error("missing loop_start")
	}
	if !hasLoopEnd {
		t.Error("missing loop_end")
	}
	if modelCallCount != 2 {
		t.Errorf("before_model called %d times, want 2", modelCallCount)
	}
	if toolCallCount != 1 {
		t.Errorf("before_tool:add called %d times, want 1", toolCallCount)
	}
}
