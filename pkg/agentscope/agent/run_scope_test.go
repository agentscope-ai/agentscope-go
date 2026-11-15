package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/runctx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
)

type lateCoreMiddleware struct {
	middleware.BaseMiddleware
	trigger chan struct{}
	done    chan struct{}
	first   atomic.Bool
}

func (m *lateCoreMiddleware) OnReply(ctx context.Context, in middleware.ReplyInput, next middleware.ReplyHandler) <-chan event.Event {
	if m.first.Swap(true) {
		return next(ctx, in)
	}
	out := make(chan event.Event)
	close(out)
	go func() {
		defer close(m.done)
		<-m.trigger
		for range next(ctx, in) {
		}
	}()
	return out
}

func TestApplicationScopeRejectsLateCore(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		name := "ordinary"
		if recovery {
			name = "recovery"
		}
		t.Run(name, func(t *testing.T) {
			m := &recoveryModel{}
			mw := &lateCoreMiddleware{trigger: make(chan struct{}), done: make(chan struct{})}
			opts := []AgentOption{WithMiddlewares(mw)}
			if recovery {
				opts = append(opts, WithReplyRecovery(), WithStateSaver(newFakeStateSaver()))
			}
			a := NewUnifiedAgent("bot", "test", m, opts...)
			ctx, scope := runctx.New(context.Background())
			out, err := a.ReplyStream(ctx, "late")
			if err != nil {
				t.Fatal(err)
			}
			for range out {
			}
			scope.Close()
			close(mw.trigger)
			select {
			case <-mw.done:
			case <-time.After(3 * time.Second):
				t.Fatal("late core did not return")
			}
			if len(m.choices) != 0 {
				t.Fatal("late core called model after owner finished")
			}
			// In recovery mode, rejecting the late invocation must not leave
			// a started recovery lease without a core to release it.
			if _, err := a.Reply(context.Background(), "fresh"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type detachedCoreMiddleware struct{ middleware.BaseMiddleware }

func (*detachedCoreMiddleware) OnReply(_ context.Context, in middleware.ReplyInput, next middleware.ReplyHandler) <-chan event.Event {
	return next(context.Background(), in)
}

func TestApplicationScopeReleasesRejectedRecoveryCore(t *testing.T) {
	a := NewUnifiedAgent("bot", "test", &recoveryModel{}, WithReplyRecovery(), WithStateSaver(newFakeStateSaver()), WithMiddlewares(&detachedCoreMiddleware{}))
	ctx, scope := runctx.New(context.Background())
	out, err := a.ReplyStream(ctx, "lost recovery context")
	if err != nil {
		t.Fatal(err)
	}
	for range out {
	}
	done := make(chan struct{})
	go func() { scope.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("rejected recovery core leaked scope lease")
	}
	if a.activeRecovery != nil {
		t.Fatal("rejected core retained recovery ownership")
	}
}
