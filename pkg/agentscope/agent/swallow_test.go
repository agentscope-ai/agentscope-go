package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// swallowEndMiddleware swallows ReplyEndEvents (Python #2322 style):
// receiving them without forwarding forces another reasoning-acting
// round. It forwards everything else, including the internal sentinel.
type swallowEndMiddleware struct {
	middleware.BaseMiddleware
	mu         sync.Mutex
	maxSwallow int // -1 = swallow forever
	swallowed  int
}

func (m *swallowEndMiddleware) shouldSwallow() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.maxSwallow >= 0 && m.swallowed >= m.maxSwallow {
		return false
	}
	m.swallowed++
	return true
}

func (m *swallowEndMiddleware) OnReply(ctx context.Context, input middleware.ReplyInput, next middleware.ReplyHandler) <-chan event.Event {
	in := next(ctx, input)
	out := make(chan event.Event, 16)
	go func() {
		defer close(out)
		for evt := range in {
			if _, ok := evt.(event.ReplyEndEvent); ok && m.shouldSwallow() {
				continue // swallow
			}
			select {
			case out <- evt:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func TestReplyEndEvent_FinishedReasonCompleted(t *testing.T) {
	mock := &mockChatModel{responses: []model.ChatResponse{
		{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "hi"}}, IsLast: true},
	}}
	a := NewUnifiedAgent("reason-agent", "helpful", mock)
	ch, err := a.ReplyStream(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	var end *event.ReplyEndEvent
	for evt := range ch {
		if e, ok := evt.(event.ReplyEndEvent); ok {
			end = &e
		}
	}
	if end == nil {
		t.Fatal("no ReplyEndEvent")
	}
	if end.FinishedReason != types.ReplyCompleted {
		t.Errorf("finished reason = %q, want completed", end.FinishedReason)
	}
	if end.Error != nil {
		t.Errorf("completed reply must carry no error, got %+v", end.Error)
	}
}

func TestMiddleware_SwallowReplyEndForcesAnotherRound(t *testing.T) {
	mock := &mockChatModel{responses: []model.ChatResponse{
		{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "first round"}}, IsLast: true},
		{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "second round"}}, IsLast: true},
	}}
	mw := &swallowEndMiddleware{maxSwallow: 1}
	a := NewUnifiedAgent("swallow-agent", "helpful", mock, WithMiddlewares(mw))

	ch, err := a.ReplyStream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var ends int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for evt := range ch {
			switch e := evt.(type) {
			case event.TextBlockDeltaEvent:
				text.WriteString(e.Delta)
			case event.ReplyEndEvent:
				ends++
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reply did not finish — swallow continuation deadlocked")
	}

	if !strings.Contains(text.String(), "first round") || !strings.Contains(text.String(), "second round") {
		t.Errorf("both rounds must be streamed, got %q", text.String())
	}
	if ends != 1 {
		t.Errorf("exactly one ReplyEndEvent may escape the chain, got %d", ends)
	}
	if mock.callCount != 2 {
		t.Errorf("model must be called once per round, got %d", mock.callCount)
	}
}

// failAfterModel answers the first n calls, then fails.
type failAfterModel struct {
	ok    int
	calls int
}

func (m *failAfterModel) Chat(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	m.calls++
	if m.calls > m.ok {
		return nil, fmt.Errorf("boom")
	}
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}},
		IsLast:  true,
	}, nil
}

func (m *failAfterModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (m *failAfterModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 10 }

func TestMiddleware_SwallowBusyLoopGuard(t *testing.T) {
	// Round 1: model OK (progress) → end swallowed. Round 2: model fails
	// (no progress) → error end swallowed → the guard must end the reply
	// instead of looping forever.
	mock := &failAfterModel{ok: 1}
	mw := &swallowEndMiddleware{maxSwallow: -1}
	a := NewUnifiedAgent("busy-agent", "helpful", mock, WithMiddlewares(mw))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := a.ReplyStream(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	sawGuard := false
	for evt := range ch {
		if ce, ok := evt.(event.CustomEvent); ok && strings.Contains(ce.Name, "swallow_loop") {
			sawGuard = true
		}
	}
	if !sawGuard {
		t.Error("busy-loop guard event missing")
	}
	if mock.calls > 3 {
		t.Errorf("guard must stop the loop promptly, model called %d times", mock.calls)
	}
}

func TestMiddleware_InterruptedEndCannotBeSwallowed(t *testing.T) {
	// A blocking model; cancel mid-reply. The interrupted end must escape
	// the swallow logic (no continuation round).
	block := make(chan struct{})
	mock := &blockingModel{block: block}
	mw := &swallowEndMiddleware{maxSwallow: -1}
	a := NewUnifiedAgent("interrupt-agent", "helpful", mock, WithMiddlewares(mw))

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := a.ReplyStream(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch {
		}
	}()
	time.Sleep(50 * time.Millisecond) // let the reply reach the model call
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not close after cancellation")
	}
	if mock.callCount() != 1 {
		t.Errorf("no continuation round may start after interruption, calls=%d", mock.callCount())
	}
}

// blockingModel blocks in Chat until the context is canceled.
type blockingModel struct {
	block chan struct{}
	mu    sync.Mutex
	calls int
}

func (m *blockingModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *blockingModel) Chat(ctx context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	select {
	case <-m.block:
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "late"}}, IsLast: true}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *blockingModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (m *blockingModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 10 }

func TestReplyEndEvent_ExceedMaxItersReason(t *testing.T) {
	// A model that always calls a tool burns every iteration; the end
	// event must carry exceed_max_iters (HARNESS R9-M1), and the
	// ExceedMaxItersEvent still fires.
	echoExecutions = 0
	toolLoop := &toolLoopModel{}
	a := NewUnifiedAgent("maxiters-agent", "helpful", toolLoop,
		WithToolkit(tool.NewToolkit(echoToolFixtureA())),
		WithReactConfig(ReactConfig{MaxIters: 2}),
	)
	ch, err := a.ReplyStream(context.Background(), "loop forever")
	if err != nil {
		t.Fatal(err)
	}
	var end *event.ReplyEndEvent
	sawExceed := false
	for evt := range ch {
		switch e := evt.(type) {
		case event.ReplyEndEvent:
			end = &e
		case event.ExceedMaxItersEvent:
			sawExceed = true
		}
	}
	if !sawExceed {
		t.Error("ExceedMaxItersEvent missing")
	}
	if end == nil || end.FinishedReason != types.ReplyExceedMaxIters {
		t.Fatalf("end reason = %v, want exceed_max_iters", end)
	}
}

// toolLoopModel answers every call with a tool invocation.
type toolLoopModel struct{ call int }

func (m *toolLoopModel) Chat(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	m.call++
	return &model.ChatResponse{
		Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: fmt.Sprintf("tc_%d", m.call), Name: "echo_a", Input: "{}", State: message.ToolCallPending},
		},
		IsLast: true,
	}, nil
}

func (m *toolLoopModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (m *toolLoopModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 10 }
