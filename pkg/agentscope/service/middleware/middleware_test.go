package middleware

import (
	"context"
	"sync"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	mw "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// --- InboxMiddleware tests ---

type mockInbox struct {
	msgs []*message.Msg
}

func (m *mockInbox) DrainMessages(_ context.Context, _ string) ([]*message.Msg, error) {
	result := m.msgs
	m.msgs = nil
	return result, nil
}

func TestInboxMiddleware_InjectsMessages(t *testing.T) {
	inbox := &mockInbox{
		msgs: []*message.Msg{
			message.UserMsg("alice", "hello from alice"),
		},
	}
	mid := NewInboxMiddleware(inbox)

	var capturedInput string
	core := func(_ context.Context, input mw.ReplyInput) <-chan event.Event {
		capturedInput = input.UserInput
		ch := make(chan event.Event)
		close(ch)
		return ch
	}

	ch := mid.OnReply(context.Background(), mw.ReplyInput{
		AgentName: "bot",
		UserInput: "original input",
	}, core)
	for range ch {
	}

	if capturedInput == "original input" {
		t.Error("expected inbox messages to be prepended to input")
	}
	if len(inbox.msgs) != 0 {
		t.Error("expected inbox to be drained")
	}
}

func TestInboxMiddleware_EmptyInbox(t *testing.T) {
	inbox := &mockInbox{}
	mid := NewInboxMiddleware(inbox)

	var capturedInput string
	core := func(_ context.Context, input mw.ReplyInput) <-chan event.Event {
		capturedInput = input.UserInput
		ch := make(chan event.Event)
		close(ch)
		return ch
	}

	ch := mid.OnReply(context.Background(), mw.ReplyInput{
		AgentName: "bot",
		UserInput: "original input",
	}, core)
	for range ch {
	}

	if capturedInput != "original input" {
		t.Error("expected input unchanged with empty inbox")
	}
}

// --- StateChangeMiddleware tests ---

type mockListener struct {
	mu      sync.Mutex
	changes []map[string]any
}

func (m *mockListener) OnStateChange(_ context.Context, _, _ string, change map[string]any) {
	m.mu.Lock()
	m.changes = append(m.changes, change)
	m.mu.Unlock()
}

func TestStateChangeMiddleware_EmitsOnSuccess(t *testing.T) {
	listener := &mockListener{}
	mid := NewStateChangeMiddleware(listener)

	core := func(_ context.Context, _ *mw.ActingInput) (*tool.ToolResponse, error) {
		return tool.NewTextResponse("ok"), nil
	}

	resp, err := mid.OnActing(context.Background(), &mw.ActingInput{
		AgentName: "agent",
		ToolCall:  message.ToolCallBlock{Name: "read", ID: "tc1"},
	}, core)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response")
	}
	if len(listener.changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(listener.changes))
	}
	if listener.changes[0]["tool"] != "read" {
		t.Errorf("expected tool=read, got %v", listener.changes[0]["tool"])
	}
}

func TestStateChangeMiddleware_NoEmitOnError(t *testing.T) {
	listener := &mockListener{}
	mid := NewStateChangeMiddleware(listener)

	core := func(_ context.Context, _ *mw.ActingInput) (*tool.ToolResponse, error) {
		return nil, context.Canceled
	}

	_, _ = mid.OnActing(context.Background(), &mw.ActingInput{
		AgentName: "agent",
		ToolCall:  message.ToolCallBlock{Name: "bash"},
	}, core)

	if len(listener.changes) != 0 {
		t.Errorf("expected 0 changes on error, got %d", len(listener.changes))
	}
}

// --- ToolOffloadMiddleware tests ---

func TestToolOffloadMiddleware_OffloadsMatching(t *testing.T) {
	var completed bool
	var mu sync.Mutex

	mid := NewToolOffloadMiddleware(
		func(name string) bool { return name == "slow_tool" },
		func(_, _, _ string, resp *tool.ToolResponse) {
			mu.Lock()
			completed = true
			mu.Unlock()
		},
	)

	core := func(_ context.Context, _ *mw.ActingInput) (*tool.ToolResponse, error) {
		return tool.NewTextResponse("done"), nil
	}

	resp, err := mid.OnActing(context.Background(), &mw.ActingInput{
		AgentName: "agent",
		ToolCall:  message.ToolCallBlock{Name: "slow_tool", ID: "tc1"},
	}, core)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultRunning {
		t.Errorf("expected ToolResultRunning, got %v", resp.State)
	}

	mid.Wait()

	mu.Lock()
	if !completed {
		t.Error("expected onComplete callback to be called")
	}
	mu.Unlock()
}

func TestToolOffloadMiddleware_PassthroughNonMatching(t *testing.T) {
	mid := NewToolOffloadMiddleware(
		func(name string) bool { return name == "slow_tool" },
		nil,
	)

	core := func(_ context.Context, _ *mw.ActingInput) (*tool.ToolResponse, error) {
		return tool.NewTextResponse("immediate"), nil
	}

	resp, err := mid.OnActing(context.Background(), &mw.ActingInput{
		AgentName: "agent",
		ToolCall:  message.ToolCallBlock{Name: "fast_tool"},
	}, core)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Errorf("expected ToolResultSuccess, got %v", resp.State)
	}
}
