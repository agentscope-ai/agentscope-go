package middleware

import (
	"context"
	"fmt"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// --- Test helpers ---

type orderTracker struct {
	order []string
}

type orderMiddleware struct {
	BaseMiddleware
	tracker *orderTracker
	label   string
}

func (m *orderMiddleware) OnReply(ctx context.Context, input ReplyInput, next ReplyHandler) <-chan event.Event {
	m.tracker.order = append(m.tracker.order, m.label+":before")
	ch := next(ctx, input)
	// Drain the channel to simulate consuming events
	for range ch {
	}
	m.tracker.order = append(m.tracker.order, m.label+":after")
	// Return a closed channel since we already consumed
	out := make(chan event.Event)
	close(out)
	return out
}

func (m *orderMiddleware) OnModelCall(ctx context.Context, input *ModelCallInput, next ModelCallHandler) (*model.ChatResponse, error) {
	m.tracker.order = append(m.tracker.order, m.label+":before")
	resp, err := next(ctx, input)
	m.tracker.order = append(m.tracker.order, m.label+":after")
	return resp, err
}

func (m *orderMiddleware) OnActing(ctx context.Context, input *ActingInput, next ActingHandler) (*tool.ToolResponse, error) {
	m.tracker.order = append(m.tracker.order, m.label+":before")
	resp, err := next(ctx, input)
	m.tracker.order = append(m.tracker.order, m.label+":after")
	return resp, err
}

func (m *orderMiddleware) OnSystemPrompt(_ context.Context, _ string, prompt string) string {
	m.tracker.order = append(m.tracker.order, m.label)
	return prompt + "[" + m.label + "]"
}

func (m *orderMiddleware) OnCompressContext(ctx context.Context, input CompressInput, next CompressHandler) error {
	m.tracker.order = append(m.tracker.order, m.label+":before")
	err := next(ctx, input)
	m.tracker.order = append(m.tracker.order, m.label+":after")
	return err
}

// --- Tests ---

func TestBuildReplyChain_OnionOrder(t *testing.T) {
	tracker := &orderTracker{}
	mw1 := &orderMiddleware{BaseMiddleware: BaseMiddleware{MiddlewareKey: "mw1"}, tracker: tracker, label: "outer"}
	mw2 := &orderMiddleware{BaseMiddleware: BaseMiddleware{MiddlewareKey: "mw2"}, tracker: tracker, label: "inner"}

	core := func(ctx context.Context, input ReplyInput) <-chan event.Event {
		tracker.order = append(tracker.order, "core")
		ch := make(chan event.Event)
		close(ch)
		return ch
	}

	chain := BuildReplyChain([]Middleware{mw1, mw2}, core)
	ch := chain(context.Background(), ReplyInput{AgentName: "test"})
	for range ch {
	}

	expected := []string{"outer:before", "inner:before", "core", "inner:after", "outer:after"}
	if len(tracker.order) != len(expected) {
		t.Fatalf("expected %d calls, got %d: %v", len(expected), len(tracker.order), tracker.order)
	}
	for i, v := range expected {
		if tracker.order[i] != v {
			t.Errorf("order[%d] = %q, want %q", i, tracker.order[i], v)
		}
	}
}

func TestBuildModelCallChain_OnionOrder(t *testing.T) {
	tracker := &orderTracker{}
	mw1 := &orderMiddleware{BaseMiddleware: BaseMiddleware{MiddlewareKey: "mw1"}, tracker: tracker, label: "outer"}
	mw2 := &orderMiddleware{BaseMiddleware: BaseMiddleware{MiddlewareKey: "mw2"}, tracker: tracker, label: "inner"}

	core := func(ctx context.Context, input *ModelCallInput) (*model.ChatResponse, error) {
		tracker.order = append(tracker.order, "core")
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "hello"}}}, nil
	}

	chain := BuildModelCallChain([]Middleware{mw1, mw2}, core)
	resp, err := chain(context.Background(), &ModelCallInput{AgentName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetTextContent() != "hello" {
		t.Errorf("unexpected response: %v", resp.Content)
	}

	expected := []string{"outer:before", "inner:before", "core", "inner:after", "outer:after"}
	if len(tracker.order) != len(expected) {
		t.Fatalf("expected %d calls, got %d: %v", len(expected), len(tracker.order), tracker.order)
	}
	for i, v := range expected {
		if tracker.order[i] != v {
			t.Errorf("order[%d] = %q, want %q", i, tracker.order[i], v)
		}
	}
}

func TestBuildActingChain_OnionOrder(t *testing.T) {
	tracker := &orderTracker{}
	mw1 := &orderMiddleware{BaseMiddleware: BaseMiddleware{MiddlewareKey: "mw1"}, tracker: tracker, label: "outer"}
	mw2 := &orderMiddleware{BaseMiddleware: BaseMiddleware{MiddlewareKey: "mw2"}, tracker: tracker, label: "inner"}

	core := func(ctx context.Context, input *ActingInput) (*tool.ToolResponse, error) {
		tracker.order = append(tracker.order, "core")
		return tool.NewTextResponse("done"), nil
	}

	chain := BuildActingChain([]Middleware{mw1, mw2}, core)
	resp, err := chain(context.Background(), &ActingInput{AgentName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}

	expected := []string{"outer:before", "inner:before", "core", "inner:after", "outer:after"}
	for i, v := range expected {
		if tracker.order[i] != v {
			t.Errorf("order[%d] = %q, want %q", i, tracker.order[i], v)
		}
	}
}

func TestApplySystemPromptPipeline(t *testing.T) {
	tracker := &orderTracker{}
	mw1 := &orderMiddleware{BaseMiddleware: BaseMiddleware{MiddlewareKey: "mw1"}, tracker: tracker, label: "A"}
	mw2 := &orderMiddleware{BaseMiddleware: BaseMiddleware{MiddlewareKey: "mw2"}, tracker: tracker, label: "B"}

	result := ApplySystemPromptPipeline(context.Background(), []Middleware{mw1, mw2}, "agent", "base")
	if result != "base[A][B]" {
		t.Errorf("got %q, want %q", result, "base[A][B]")
	}
	// Pipeline order: A then B
	expected := []string{"A", "B"}
	for i, v := range expected {
		if tracker.order[i] != v {
			t.Errorf("order[%d] = %q, want %q", i, tracker.order[i], v)
		}
	}
}

func TestBaseMiddleware_PassThrough(t *testing.T) {
	mw := &BaseMiddleware{MiddlewareKey: "base"}

	// OnReply pass-through
	coreCalled := false
	core := func(ctx context.Context, input ReplyInput) <-chan event.Event {
		coreCalled = true
		ch := make(chan event.Event)
		close(ch)
		return ch
	}
	ch := mw.OnReply(context.Background(), ReplyInput{}, core)
	for range ch {
	}
	if !coreCalled {
		t.Error("OnReply did not pass through to core")
	}

	// OnModelCall pass-through
	modelCore := func(ctx context.Context, input *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{}, nil
	}
	_, err := mw.OnModelCall(context.Background(), &ModelCallInput{}, modelCore)
	if err != nil {
		t.Errorf("OnModelCall pass-through error: %v", err)
	}

	// OnSystemPrompt pass-through
	prompt := mw.OnSystemPrompt(context.Background(), "agent", "hello")
	if prompt != "hello" {
		t.Errorf("OnSystemPrompt changed prompt: %q", prompt)
	}

	// Key
	if mw.Key() != "base" {
		t.Errorf("Key() = %q, want %q", mw.Key(), "base")
	}
}

// shortCircuitMiddleware stops the chain by not calling next.
type shortCircuitMiddleware struct {
	BaseMiddleware
}

func (m *shortCircuitMiddleware) OnModelCall(_ context.Context, _ *ModelCallInput, _ ModelCallHandler) (*model.ChatResponse, error) {
	return nil, fmt.Errorf("blocked by middleware")
}

func TestBuildModelCallChain_ShortCircuit(t *testing.T) {
	blocker := &shortCircuitMiddleware{BaseMiddleware: BaseMiddleware{MiddlewareKey: "blocker"}}

	coreCalled := false
	core := func(ctx context.Context, input *ModelCallInput) (*model.ChatResponse, error) {
		coreCalled = true
		return &model.ChatResponse{}, nil
	}

	chain := BuildModelCallChain([]Middleware{blocker}, core)
	_, err := chain(context.Background(), &ModelCallInput{})
	if err == nil {
		t.Fatal("expected error from short-circuit middleware")
	}
	if coreCalled {
		t.Error("core was called despite short-circuit")
	}
}

func TestBuildCompressChain_OnionOrder(t *testing.T) {
	tracker := &orderTracker{}
	mw1 := &orderMiddleware{BaseMiddleware: BaseMiddleware{MiddlewareKey: "mw1"}, tracker: tracker, label: "outer"}
	mw2 := &orderMiddleware{BaseMiddleware: BaseMiddleware{MiddlewareKey: "mw2"}, tracker: tracker, label: "inner"}

	core := func(ctx context.Context, input CompressInput) error {
		tracker.order = append(tracker.order, "core")
		return nil
	}

	chain := BuildCompressChain([]Middleware{mw1, mw2}, core)
	err := chain(context.Background(), CompressInput{AgentName: "test"})
	if err != nil {
		t.Fatal(err)
	}

	expected := []string{"outer:before", "inner:before", "core", "inner:after", "outer:after"}
	for i, v := range expected {
		if tracker.order[i] != v {
			t.Errorf("order[%d] = %q, want %q", i, tracker.order[i], v)
		}
	}
}

func TestMiddleContext_GetSet(t *testing.T) {
	mc := MiddleContext{}
	mc.Set("myMiddleware", "counter", 42)

	v, ok := mc.Get("myMiddleware", "counter")
	if !ok || v != 42 {
		t.Errorf("Get returned %v, %v", v, ok)
	}

	// Missing key
	_, ok = mc.Get("other", "counter")
	if ok {
		t.Error("expected false for missing middleware key")
	}

	_, ok = mc.Get("myMiddleware", "missing")
	if ok {
		t.Error("expected false for missing field")
	}
}

func TestWithMiddleContext_RoundTrip(t *testing.T) {
	mc := MiddleContext{}
	mc.Set("budget", "spent", 100)

	ctx := WithMiddleContext(context.Background(), mc)
	got := GetMiddleContext(ctx)
	if got == nil {
		t.Fatal("nil MiddleContext from context")
	}

	v, ok := got.Get("budget", "spent")
	if !ok || v != 100 {
		t.Errorf("round-trip failed: %v, %v", v, ok)
	}
}

func TestGetMiddleContext_NilWhenMissing(t *testing.T) {
	mc := GetMiddleContext(context.Background())
	if mc != nil {
		t.Error("expected nil MiddleContext from bare context")
	}
}
