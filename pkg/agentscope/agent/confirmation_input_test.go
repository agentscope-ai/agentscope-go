package agent

import (
	"context"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/confirmation"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/runctx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func TestReplyLocalConfirmationRetiresCanceledQueue(t *testing.T) {
	tc := message.ToolCallBlock{Type: "tool_call", ID: "reused-call", Name: "test_tool", Input: `{}`, State: message.ToolCallPending}
	toolResponse := model.ChatResponse{Content: []message.ContentBlock{tc}, IsLast: true}
	cm := &mockChatModel{responses: []model.ChatResponse{toolResponse, toolResponse, {Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "finished"}}, IsLast: true}}}
	a := NewUnifiedAgent("owned", "fixture", cm, WithToolkit(tool.NewToolkit(newHITLMockTool("test_tool", false))), WithPermissionContext(permission.NewContext(permission.ModeDefault)), WithModelStreaming(false))
	oldInput := make(chan event.UserConfirmResultEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	firstCtx, scope := runctx.New(confirmation.WithInput(ctx, a, oldInput))
	ch, err := a.ReplyStream(firstCtx, "first")
	if err != nil {
		t.Fatal(err)
	}
	var first event.RequireUserConfirmEvent
	for e := range ch {
		if req, ok := e.(event.RequireUserConfirmEvent); ok {
			first = req
			cancel()
		}
	}
	cancel()
	scope.Close()
	if len(first.ToolCalls) != 1 {
		t.Fatal("first reply never parked")
	}
	// A sender may win its enqueue select concurrently with cancellation. That
	// accepted result belongs to the retired source, never a later reply.
	oldInput <- event.NewUserConfirmResultEvent(first.ReplyID, []event.ConfirmResult{{Confirmed: true, ToolCall: first.ToolCalls[0]}})
	newInput := make(chan event.UserConfirmResultEvent, 1)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	secondCtx, scope2 := runctx.New(confirmation.WithInput(ctx2, a, newInput))
	defer scope2.Close()
	ch, err = a.ReplyStream(secondCtx, "second")
	if err != nil {
		t.Fatal(err)
	}
	gotPending, gotDenied := false, false
	for e := range ch {
		switch e := e.(type) {
		case event.RequireUserConfirmEvent:
			gotPending = true
			if e.ToolCalls[0].ID != tc.ID || len(oldInput) != 1 {
				t.Fatal("retired confirmation source was consumed")
			}
			newInput <- event.NewUserConfirmResultEvent(e.ReplyID, []event.ConfirmResult{{Confirmed: false, ToolCall: e.ToolCalls[0]}})
		case event.ToolResultEndEvent:
			gotDenied = e.State == message.ToolResultDenied
		}
	}
	if !gotPending || !gotDenied || ctx2.Err() != nil || len(oldInput) != 1 {
		t.Fatalf("pending=%v denied=%v ctx=%v retired=%d", gotPending, gotDenied, ctx2.Err(), len(oldInput))
	}
}

func TestReplyLocalConfirmationDoesNotOverrideAnotherAgent(t *testing.T) {
	a := NewUnifiedAgent("parent", "fixture", &mockChatModel{}, WithPermissionContext(permission.NewContext(permission.ModeDefault)))
	b := NewUnifiedAgent("child", "fixture", &mockChatModel{}, WithPermissionContext(permission.NewContext(permission.ModeDefault)))
	input := make(chan event.UserConfirmResultEvent, 1)
	input <- event.NewUserConfirmResultEvent("parent", []event.ConfirmResult{{Confirmed: false, ToolCall: message.ToolCallBlock{ID: "same"}}})
	ctx, cancel := context.WithTimeout(confirmation.WithInput(context.Background(), a, input), 5*time.Second)
	defer cancel()
	b.SubmitUserConfirm(&event.UserConfirmResultEvent{ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: message.ToolCallBlock{ID: "same", Name: "child-tool"}}}})
	ok, tc := b.waitForConfirmation(ctx, "same")
	if !ok || tc.Name != "child-tool" || len(input) != 1 {
		t.Fatalf("child consumed parent source: ok=%v call=%+v remaining=%d", ok, tc, len(input))
	}
}

func TestReplyLocalClosedConfirmationSourceDenies(t *testing.T) {
	a := NewUnifiedAgent("owned", "fixture", &mockChatModel{}, WithPermissionContext(permission.NewContext(permission.ModeDefault)))
	input := make(chan event.UserConfirmResultEvent)
	close(input)
	ctx, cancel := context.WithTimeout(confirmation.WithInput(context.Background(), a, input), time.Second)
	defer cancel()
	ok, _ := a.waitForConfirmation(ctx, "call")
	if ok || ctx.Err() != nil {
		t.Fatalf("closed source did not deny promptly: ok=%v ctx=%v", ok, ctx.Err())
	}
}
