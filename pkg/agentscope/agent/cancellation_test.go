package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

type cancellationModel struct {
	model.ChatModel
	chat func(context.Context) (*model.ChatResponse, error)
}

func (m cancellationModel) Chat(ctx context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	return m.chat(ctx)
}
func (m cancellationModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

func TestReplyCancellationDoesNotReturnHistory(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			var calls atomic.Int32
			m := cancellationModel{chat: func(ctx context.Context) (*model.ChatResponse, error) {
				if calls.Add(1) == 1 {
					return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "previous"}}}, nil
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			a := NewUnifiedAgent("bot", "test", m)
			if _, err := a.Reply(context.Background(), "first"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			want := error(context.Canceled)
			if deadline {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				want = context.DeadlineExceeded
			}
			reply, err := a.Reply(ctx, "second")
			if !errors.Is(err, want) || reply != nil {
				t.Fatalf("reply=%v err=%v, want nil, %v", reply, err, want)
			}
		})
	}
}

func TestModelCancellationStopsRetryAndFallback(t *testing.T) {
	for _, bridge := range []bool{false, true} {
		for _, mode := range []string{"during_call", "backoff", "wrapped", "wrapped_deadline"} {
			t.Run(fmt.Sprintf("bridge=%t/%s", bridge, mode), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var primary, fallback atomic.Int32
				first := make(chan struct{})
				m := cancellationModel{chat: func(context.Context) (*model.ChatResponse, error) {
					if primary.Add(1) == 1 {
						close(first)
					}
					if mode == "during_call" {
						cancel()
					}
					if mode == "wrapped" {
						return nil, fmt.Errorf("provider: %w", context.Canceled)
					}
					if mode == "wrapped_deadline" {
						return nil, fmt.Errorf("provider: %w", context.DeadlineExceeded)
					}
					return nil, errors.New("unavailable")
				}}
				f := cancellationModel{chat: func(context.Context) (*model.ChatResponse, error) {
					fallback.Add(1)
					return nil, errors.New("fallback")
				}}
				a := NewUnifiedAgent("bot", "test", m, WithModelConfig(ModelConfig{MaxRetries: 3, RetryDelay: 5 * time.Second, FallbackModel: f}))
				if mode == "backoff" {
					go func() { <-first; cancel() }()
				}
				start := time.Now()
				var err error
				if bridge {
					_, err = loop.New(NewUnifiedAgentRunner(a).LoopOptions()...).RunSync(ctx, "test")
				} else {
					_, err = a.callModel(ctx, nil, nil)
				}
				if primary.Load() != 1 || fallback.Load() != 0 {
					t.Errorf("calls primary=%d fallback=%d", primary.Load(), fallback.Load())
				}
				if time.Since(start) > 2*time.Second {
					t.Errorf("cancellation waited for retry delay")
				}
				want := error(context.Canceled)
				if mode == "wrapped_deadline" {
					want = context.DeadlineExceeded
				}
				if !bridge && !errors.Is(err, want) {
					t.Errorf("err=%v", err)
				}
			})
		}
	}
}

func TestModelRetryAndFallbackSuccess(t *testing.T) {
	for _, useFallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%t", useFallback), func(t *testing.T) {
			var primaryCalls, fallbackCalls int
			want := &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "recovered"}}}
			primary := cancellationModel{chat: func(context.Context) (*model.ChatResponse, error) {
				primaryCalls++
				if !useFallback && primaryCalls == 2 {
					return want, nil
				}
				return nil, errors.New("temporary failure")
			}}
			fallback := cancellationModel{chat: func(context.Context) (*model.ChatResponse, error) {
				fallbackCalls++
				return want, nil
			}}
			a := NewUnifiedAgent("bot", "test", primary, WithModelConfig(ModelConfig{
				MaxRetries: 2, RetryDelay: time.Nanosecond, FallbackModel: fallback,
			}))
			got, err := a.callModel(context.Background(), nil, nil)
			wantFallbackCalls := 0
			if useFallback {
				wantFallbackCalls = 1
			}
			if err != nil || got != want || primaryCalls != 2 || fallbackCalls != wantFallbackCalls {
				t.Fatalf("response=%v err=%v primary=%d fallback=%d", got, err, primaryCalls, fallbackCalls)
			}
		})
	}
}

func TestModelFallbackCancellationDiscardsResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	primary := cancellationModel{chat: func(context.Context) (*model.ChatResponse, error) {
		return nil, errors.New("primary unavailable")
	}}
	fallback := cancellationModel{chat: func(context.Context) (*model.ChatResponse, error) {
		cancel()
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "too late"}}}, nil
	}}
	a := NewUnifiedAgent("bot", "test", primary, WithModelConfig(ModelConfig{MaxRetries: 1, FallbackModel: fallback}))
	got, err := a.callModel(ctx, nil, nil)
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("fallback response=%v error=%v; want nil and context.Canceled", got, err)
	}
}

type cancelAtReplyEnd struct {
	middleware.BaseMiddleware
	cancel context.CancelFunc
	done   chan struct{}
}

func (m *cancelAtReplyEnd) OnReply(ctx context.Context, in middleware.ReplyInput, next middleware.ReplyHandler) <-chan event.Event {
	ch := next(ctx, in)
	out := make(chan event.Event)
	go func() {
		defer close(out)
		defer close(m.done)
		for e := range ch {
			select {
			case out <- e:
			case <-ctx.Done():
				return
			}
			if _, ok := e.(event.ReplyEndEvent); ok {
				m.cancel()
			}
		}
	}()
	return out
}
func TestReplyChecksCancellationAfterTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	a := NewUnifiedAgent("bot", "test", &mockChatModel{responses: []model.ChatResponse{{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "completed"}}}}}, WithMiddlewares(&cancelAtReplyEnd{cancel: cancel, done: done}))
	reply, err := a.Reply(ctx, "test")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("middleware did not stop")
	}
	if reply != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("terminal suppressed cancellation: reply=%v err=%v", reply, err)
	}
}
func TestReplyNoNewResponseDoesNotReturnHistory(t *testing.T) {
	a := NewUnifiedAgent("bot", "test", &mockChatModel{responses: []model.ChatResponse{{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "previous"}}}, {}}})
	if _, err := a.Reply(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if reply, err := a.Reply(context.Background(), "empty"); reply != nil || err == nil {
		t.Fatalf("old response reused: %v %v", reply, err)
	}
}

func (m cancellationModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
