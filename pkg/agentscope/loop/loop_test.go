// pkg/agentscope/loop/loop_test.go
package loop

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func TestValidTransitions(t *testing.T) {
	valid := []struct {
		from, to protocol.LoopState
	}{
		{protocol.StateReason, protocol.StateInspect},
		{protocol.StateInspect, protocol.StateAct},
		{protocol.StateInspect, protocol.StateWait},
		{protocol.StateInspect, protocol.StateExit},
		{protocol.StateInspect, protocol.StateReason},
		{protocol.StateAct, protocol.StateReason},
		{protocol.StateAct, protocol.StateExit},
		{protocol.StateWait, protocol.StateReason},
	}
	for _, tc := range valid {
		if !IsValidTransition(tc.from, tc.to) {
			t.Errorf("transition %s -> %s should be valid", tc.from, tc.to)
		}
	}
}

func TestInvalidTransitions(t *testing.T) {
	invalid := []struct {
		from, to protocol.LoopState
	}{
		{protocol.StateReason, protocol.StateAct},
		{protocol.StateReason, protocol.StateWait},
		{protocol.StateAct, protocol.StateInspect},
		{protocol.StateWait, protocol.StateAct},
		{protocol.StateExit, protocol.StateReason},
		{protocol.StateExit, protocol.StateAct},
	}
	for _, tc := range invalid {
		if IsValidTransition(tc.from, tc.to) {
			t.Errorf("transition %s -> %s should be invalid", tc.from, tc.to)
		}
	}
}

func TestInspectResponseNoToolCalls(t *testing.T) {
	result := InspectResponse(nil)
	if result != InspectNoTools {
		t.Errorf("InspectResponse(nil) = %v, want InspectNoTools", result)
	}
}

func TestHookRunnerCallsAllHooks(t *testing.T) {
	var calls []string
	h1 := &testHook{name: "h1", calls: &calls}
	h2 := &testHook{name: "h2", calls: &calls}

	runner := NewHookRunner(h1, h2)

	runner.BeforeModelCall(protocol.StateReason, 0)
	runner.AfterModelCall(protocol.StateReason, 0, nil)

	want := []string{"h1:before_model", "h2:before_model", "h1:after_model", "h2:after_model"}
	if len(calls) != len(want) {
		t.Fatalf("got %d calls, want %d: %v", len(calls), len(want), calls)
	}
	for i, w := range want {
		if calls[i] != w {
			t.Errorf("call[%d] = %q, want %q", i, calls[i], w)
		}
	}
}

func TestHookRunnerEmptyIsNoop(t *testing.T) {
	runner := NewHookRunner()
	// Should not panic
	runner.BeforeModelCall(protocol.StateReason, 0)
	runner.AfterModelCall(protocol.StateReason, 0, nil)
	runner.BeforeToolExec(protocol.StateAct, 0, "bash")
	runner.AfterToolExec(protocol.StateAct, 0, "bash", nil)
	runner.OnStateTransition(protocol.StateReason, protocol.StateInspect, 0)
	runner.OnLoopStart()
	runner.OnLoopEnd(nil)
}

type testHook struct {
	name  string
	calls *[]string
}

func (h *testHook) BeforeModelCall(state protocol.LoopState, iter int) {
	*h.calls = append(*h.calls, h.name+":before_model")
}

func (h *testHook) AfterModelCall(state protocol.LoopState, iter int, err error) {
	*h.calls = append(*h.calls, h.name+":after_model")
}

func (h *testHook) BeforeToolExec(state protocol.LoopState, iter int, toolName string) {
	*h.calls = append(*h.calls, h.name+":before_tool")
}

func (h *testHook) AfterToolExec(state protocol.LoopState, iter int, toolName string, err error) {
	*h.calls = append(*h.calls, h.name+":after_tool")
}

func (h *testHook) OnStateTransition(from, to protocol.LoopState, iter int) {
	*h.calls = append(*h.calls, h.name+":transition")
}

func (h *testHook) OnLoopStart() {
	*h.calls = append(*h.calls, h.name+":start")
}

func (h *testHook) OnLoopEnd(err error) {
	*h.calls = append(*h.calls, h.name+":end")
}

func TestRunSyncSimpleChat(t *testing.T) {
	mc := &mockModelCaller{
		responses: []*model.ChatResponse{
			{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "Hello!"}}, IsLast: true},
		},
	}

	l := New(WithModelCaller(mc), WithMaxIters(5))
	result, err := l.RunSync(context.Background(), "Hi")
	if err != nil {
		t.Fatalf("RunSync error: %v", err)
	}
	if result.GetTextContent() != "Hello!" {
		t.Errorf("text = %q, want %q", result.GetTextContent(), "Hello!")
	}
	if mc.callCount != 1 {
		t.Errorf("model called %d times, want 1", mc.callCount)
	}
}

func TestRunSyncWithToolCall(t *testing.T) {
	mc := &mockModelCaller{
		responses: []*model.ChatResponse{
			{Content: []message.ContentBlock{
				message.ToolCallBlock{Type: "tool_use", ID: "tc1", Name: "bash", Input: `{"command":"ls"}`, State: message.ToolCallPending},
			}, IsLast: true},
			{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "Done."}}, IsLast: true},
		},
	}
	te := &mockToolExecutor{
		result: &tool.ToolResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "file.go"}},
			State:   message.ToolResultSuccess,
		},
	}

	l := New(WithModelCaller(mc), WithToolExecutor(te), WithMaxIters(5))
	result, err := l.RunSync(context.Background(), "list files")
	if err != nil {
		t.Fatalf("RunSync error: %v", err)
	}
	if result.GetTextContent() != "Done." {
		t.Errorf("text = %q, want %q", result.GetTextContent(), "Done.")
	}
	if mc.callCount != 2 {
		t.Errorf("model called %d times, want 2", mc.callCount)
	}
	if te.callCount != 1 {
		t.Errorf("tool called %d times, want 1", te.callCount)
	}
}

func TestRunSyncMaxIters(t *testing.T) {
	mc := &mockModelCaller{
		responses: []*model.ChatResponse{
			{Content: []message.ContentBlock{
				message.ToolCallBlock{Type: "tool_use", ID: "tc1", Name: "bash", Input: `{}`, State: message.ToolCallPending},
			}, IsLast: true},
			{Content: []message.ContentBlock{
				message.ToolCallBlock{Type: "tool_use", ID: "tc2", Name: "bash", Input: `{}`, State: message.ToolCallPending},
			}, IsLast: true},
			{Content: []message.ContentBlock{
				message.ToolCallBlock{Type: "tool_use", ID: "tc3", Name: "bash", Input: `{}`, State: message.ToolCallPending},
			}, IsLast: true},
		},
	}
	te := &mockToolExecutor{
		result: &tool.ToolResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}},
			State:   message.ToolResultSuccess,
		},
	}

	l := New(WithModelCaller(mc), WithToolExecutor(te), WithMaxIters(2))
	_, err := l.RunSync(context.Background(), "do stuff")
	if err != nil {
		t.Fatalf("RunSync error: %v", err)
	}
	if mc.callCount != 2 {
		t.Errorf("model called %d times, want 2 (max iters)", mc.callCount)
	}
}

func TestRunSyncContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	mc := &mockModelCaller{
		responses: []*model.ChatResponse{
			{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "never"}}, IsLast: true},
		},
	}

	l := New(WithModelCaller(mc))
	_, err := l.RunSync(ctx, "hello")
	if err == nil {
		t.Error("expected error from canceled context")
	}
}

func TestRunEmitsEvents(t *testing.T) {
	mc := &mockModelCaller{
		responses: []*model.ChatResponse{
			{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "Hi"}}, IsLast: true},
		},
	}

	l := New(WithModelCaller(mc))
	var events []event.Event
	for ev := range l.Run(context.Background(), "hello") {
		events = append(events, ev)
	}

	types := make(map[event.EventType]bool)
	for _, ev := range events {
		types[ev.GetEventType()] = true
	}
	for _, want := range []event.EventType{event.EventReplyStart, event.EventModelCallStart, event.EventModelCallEnd, event.EventReplyEnd} {
		if !types[want] {
			t.Errorf("missing event type: %s", want)
		}
	}
}

func TestRunSyncHooksCalledInOrder(t *testing.T) {
	var calls []string
	h := &testHook{name: "h", calls: &calls}

	mc := &mockModelCaller{
		responses: []*model.ChatResponse{
			{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true},
		},
	}

	l := New(WithModelCaller(mc), WithHooks(h))
	_, _ = l.RunSync(context.Background(), "test")

	expected := []string{"h:start", "h:before_model", "h:after_model", "h:transition", "h:transition", "h:end"}
	if len(calls) != len(expected) {
		t.Fatalf("got calls %v, want %v", calls, expected)
	}
	for i, want := range expected {
		if calls[i] != want {
			t.Errorf("call[%d] = %q, want %q", i, calls[i], want)
		}
	}
}

func TestRunSyncMissingToolResultBackfill(t *testing.T) {
	mc := &mockModelCaller{
		responses: []*model.ChatResponse{
			{Content: []message.ContentBlock{
				message.ToolCallBlock{Type: "tool_use", ID: "tc1", Name: "bash", Input: `{}`, State: message.ToolCallPending},
			}, IsLast: true},
		},
	}
	te := &mockToolExecutor{err: fmt.Errorf("tool crashed")}

	l := New(WithModelCaller(mc), WithToolExecutor(te), WithMaxIters(1))
	_, _ = l.RunSync(context.Background(), "run")

	msgs := l.cfg.ContextManager.Messages()
	found := false
	for _, m := range msgs {
		for _, b := range m.Content {
			if tr, ok := b.(message.ToolResultBlock); ok && tr.ID == "tc1" {
				found = true
			}
		}
	}
	if !found {
		t.Error("missing tool result backfill for tc1")
	}
}

// --- Mock implementations ---

type mockModelCaller struct {
	mu        sync.Mutex
	responses []*model.ChatResponse
	callCount int
}

func (m *mockModelCaller) Call(_ context.Context, _ []*message.Msg, _ []model.ToolSchema) (*model.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := m.callCount
	m.callCount++
	if idx >= len(m.responses) {
		return &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "(no more responses)"}},
			IsLast:  true,
		}, nil
	}
	return m.responses[idx], nil
}

type mockToolExecutor struct {
	mu        sync.Mutex
	result    *tool.ToolResponse
	err       error
	callCount int
}

func (m *mockToolExecutor) Execute(_ context.Context, _ message.ToolCallBlock) (*tool.ToolResponse, error) { //nolint:gocritic // interface
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callCount++
	if m.err != nil {
		return nil, m.err
	}
	return m.result, nil
}

func (m *mockToolExecutor) BatchExecute(ctx context.Context, calls []message.ToolCallBlock) []*ToolResult {
	var results []*ToolResult
	for _, c := range calls {
		resp, err := m.Execute(ctx, c)
		results = append(results, &ToolResult{Call: c, Response: resp, Err: err})
	}
	return results
}
