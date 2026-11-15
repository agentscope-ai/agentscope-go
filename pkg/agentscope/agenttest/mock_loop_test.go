package agenttest

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestScriptedModelCallerReturnsInOrder(t *testing.T) {
	mc := NewScriptedModelCaller(
		&model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "first"}}, IsLast: true},
		&model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "second"}}, IsLast: true},
	)

	r1, err := mc.Call(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r1.GetTextContent() != "first" {
		t.Errorf("got %q, want %q", r1.GetTextContent(), "first")
	}

	r2, _ := mc.Call(context.Background(), nil, nil)
	if r2.GetTextContent() != "second" {
		t.Errorf("got %q, want %q", r2.GetTextContent(), "second")
	}

	if mc.CallCount() != 2 {
		t.Errorf("CallCount = %d, want 2", mc.CallCount())
	}
}

func TestScriptedModelCallerExhausted(t *testing.T) {
	mc := NewScriptedModelCaller(
		&model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "only"}}, IsLast: true},
	)

	mc.Call(context.Background(), nil, nil)
	r2, err := mc.Call(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r2.GetTextContent() != "(script exhausted)" {
		t.Errorf("got %q after exhaustion", r2.GetTextContent())
	}
}

func TestSimpleToolExecutorReturnsOutput(t *testing.T) {
	te := NewSimpleToolExecutor("result text")
	resp, err := te.Execute(context.Background(), message.ToolCallBlock{ID: "tc1", Name: "bash"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) == 0 {
		t.Fatal("empty content")
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatal("expected TextBlock")
	}
	if tb.Text != "result text" {
		t.Errorf("got %q, want %q", tb.Text, "result text")
	}
	if te.CallCount() != 1 {
		t.Errorf("CallCount = %d, want 1", te.CallCount())
	}
}

func TestAssertEventPresent(t *testing.T) {
	events := []event.Event{
		event.NewReplyStartEvent("", "r1", "", message.RoleAssistant),
		event.NewModelCallStartEvent("r1", ""),
		event.NewModelCallEndEvent("r1", 10, 20),
		event.NewReplyEndEvent("", "r1"),
	}

	AssertEventPresent(t, events, event.EventReplyStart)
	AssertEventPresent(t, events, event.EventModelCallStart)
	AssertEventPresent(t, events, event.EventModelCallEnd)
	AssertEventPresent(t, events, event.EventReplyEnd)
}

func TestCollectEvents(t *testing.T) {
	ch := make(chan event.Event, 3)
	ch <- event.NewReplyStartEvent("", "r1", "", message.RoleAssistant)
	ch <- event.NewModelCallStartEvent("r1", "")
	ch <- event.NewReplyEndEvent("", "r1")
	close(ch)

	events := CollectEvents(ch)
	if len(events) != 3 {
		t.Errorf("got %d events, want 3", len(events))
	}
}

func TestAssertNoMissingToolResults(t *testing.T) {
	mc := NewScriptedModelCaller(
		&model.ChatResponse{
			Content: []message.ContentBlock{
				message.ToolCallBlock{Type: "tool_use", ID: "tc1", Name: "bash", State: message.ToolCallPending},
			},
			IsLast: true,
		},
		&model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}},
			IsLast:  true,
		},
	)
	te := NewSimpleToolExecutor("ok")

	l := loop.New(loop.WithModelCaller(mc), loop.WithToolExecutor(te), loop.WithMaxIters(5))
	events := CollectEvents(l.Run(context.Background(), "test"))

	AssertNoMissingToolResults(t, events)
}
