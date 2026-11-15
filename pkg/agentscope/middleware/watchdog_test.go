package middleware

import (
	"context"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestWatchdog_IdleTimeoutAbortsStalledReply(t *testing.T) {
	m := NewReplyWatchdog(WithReplyIdleTimeout(50 * time.Millisecond))

	core := func(ctx context.Context, _ ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, 4)
		go func() {
			defer close(ch)
			ch <- event.NewReplyStartEvent("s", "r", "a", message.RoleAssistant)
			select {
			case <-time.After(2 * time.Second): // stall far beyond idle
				ch <- event.NewReplyEndEvent("s", "r")
			case <-ctx.Done():
				// Producer honors cancellation promptly.
			}
		}()
		return ch
	}

	start := time.Now()
	out := m.OnReply(context.Background(), ReplyInput{AgentName: "a"}, core)
	n := 0
	for range out {
		n++
	}
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatalf("idle abort took too long: %v", elapsed)
	}
	if n != 1 {
		t.Errorf("events before abort = %d, want 1", n)
	}
}

func TestWatchdog_ActiveStreamSurvivesIdleWindow(t *testing.T) {
	m := NewReplyWatchdog(WithReplyIdleTimeout(80 * time.Millisecond))

	core := func(ctx context.Context, _ ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, 8)
		go func() {
			defer close(ch)
			ch <- event.NewReplyStartEvent("s", "r", "a", message.RoleAssistant)
			for i := 0; i < 5; i++ {
				select {
				case <-ctx.Done():
					return
				case <-time.After(20 * time.Millisecond): // below idle
					ch <- event.NewTextBlockDeltaEvent("r", "b", "x")
				}
			}
			ch <- event.NewReplyEndEvent("s", "r")
		}()
		return ch
	}

	out := m.OnReply(context.Background(), ReplyInput{AgentName: "a"}, core)
	n := 0
	for range out {
		n++
	}
	if n != 7 { // start + 5 deltas + end
		t.Errorf("events = %d, want 7 (stream must not be cut)", n)
	}
}

func TestWatchdog_WallClockAbortsLongReply(t *testing.T) {
	m := NewReplyWatchdog(WithReplyWallClock(60 * time.Millisecond))

	core := func(ctx context.Context, _ ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, 64)
		go func() {
			defer close(ch)
			ch <- event.NewReplyStartEvent("s", "r", "a", message.RoleAssistant)
			for i := 0; i < 100; i++ {
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Millisecond):
					select {
					case ch <- event.NewTextBlockDeltaEvent("r", "b", "x"):
					case <-ctx.Done():
						return
					}
				}
			}
			ch <- event.NewReplyEndEvent("s", "r")
		}()
		return ch
	}

	start := time.Now()
	out := m.OnReply(context.Background(), ReplyInput{AgentName: "a"}, core)
	for range out {
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("wall clock abort too slow: %v", elapsed)
	}
}

func TestWatchdog_DisabledPassthrough(t *testing.T) {
	m := NewReplyWatchdog()
	core := func(_ context.Context, _ ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, 1)
		ch <- event.NewReplyEndEvent("s", "r")
		close(ch)
		return ch
	}
	out := m.OnReply(context.Background(), ReplyInput{AgentName: "a"}, core)
	n := 0
	for range out {
		n++
	}
	if n != 1 {
		t.Errorf("events = %d", n)
	}
}
