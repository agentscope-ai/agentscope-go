package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// recordingLoopModel always requests a (nonexistent) tool unless tools are
// disabled, in which case it returns the final summary text. It records the
// ToolChoice of every call.
type recordingLoopModel struct {
	mu      sync.Mutex
	choices []string
	calls   int
}

func (m *recordingLoopModel) Chat(_ context.Context, _ []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	co := &model.CallOptions{}
	for _, o := range opts {
		o(co)
	}
	m.mu.Lock()
	m.calls++
	mode := ""
	if co.ToolChoice != nil {
		mode = co.ToolChoice.Mode
	}
	m.choices = append(m.choices, mode)
	n := m.calls
	m.mu.Unlock()

	if mode == "none" {
		return &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "final summary of work"}},
			IsLast:  true,
		}, nil
	}
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.ToolCallBlock{
			Type: "tool_call", ID: fmt.Sprintf("tc%d", n), Name: "ghost_tool", Input: "{}",
			State: message.ToolCallPending,
		}},
		IsLast: true,
	}, nil
}

func (m *recordingLoopModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (m *recordingLoopModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 10 }

// Upstream #2443: after the react budget is exhausted the agent gets ONE
// forced tools-disabled finalization call so the reply ends with a text
// summary instead of silence; the finished reason stays exceed_max_iters.
func TestForcedFinalSummaryAfterMaxIters(t *testing.T) {
	mock := &recordingLoopModel{}
	a := NewUnifiedAgent("maxit", "sys", mock,
		WithReactConfig(ReactConfig{MaxIters: 2}),
	)
	ch, err := a.ReplyStream(context.Background(), "do work")
	if err != nil {
		t.Fatal(err)
	}
	var end *event.ReplyEndEvent
	sawExceed := false
	sawSummaryText := false
	for evt := range ch {
		switch e := evt.(type) {
		case event.ReplyEndEvent:
			end = &e
		case event.ExceedMaxItersEvent:
			sawExceed = true
		case event.TextBlockDeltaEvent:
			if e.Delta == "final summary of work" {
				sawSummaryText = true
			}
		}
	}
	if !sawExceed {
		t.Error("ExceedMaxItersEvent missing")
	}
	if end == nil || end.FinishedReason != types.ReplyExceedMaxIters {
		t.Errorf("end = %+v, want exceed_max_iters", end)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.calls != 3 {
		t.Errorf("model calls = %d, want 3 (2 loop + 1 forced)", mock.calls)
	}
	last := mock.choices[len(mock.choices)-1]
	if last != "none" {
		t.Errorf("forced call ToolChoice = %q, want none", last)
	}
	if !sawSummaryText {
		t.Error("forced summary text was not streamed to consumers")
	}
}
