package middleware

import (
	"context"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
)

// ReplyWatchdogMiddleware aborts replies that run too long or stall
// (HARNESS_DESIGN E2). Two independent limits:
//   - wall clock: total time budget for one reply;
//   - idle timeout: maximum gap between consecutive events.
//
// On expiry the reply context is canceled; the agent's ctx-aware emission
// stops promptly and the surfaced error follows the normal cancellation
// path. Zero values disable the respective limit.
type ReplyWatchdogMiddleware struct {
	BaseMiddleware
	wallClock time.Duration
	idle      time.Duration
}

// WatchdogOption configures NewReplyWatchdog.
type WatchdogOption func(*ReplyWatchdogMiddleware)

// WithReplyWallClock sets the total time budget per reply.
func WithReplyWallClock(d time.Duration) WatchdogOption {
	return func(m *ReplyWatchdogMiddleware) { m.wallClock = d }
}

// WithReplyIdleTimeout sets the maximum silence between events.
func WithReplyIdleTimeout(d time.Duration) WatchdogOption {
	return func(m *ReplyWatchdogMiddleware) { m.idle = d }
}

// NewReplyWatchdog creates the watchdog middleware.
func NewReplyWatchdog(opts ...WatchdogOption) *ReplyWatchdogMiddleware {
	m := &ReplyWatchdogMiddleware{
		BaseMiddleware: BaseMiddleware{MiddlewareKey: "reply-watchdog"},
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// OnReply wraps the reply stream with wall-clock and idle timers.
func (m *ReplyWatchdogMiddleware) OnReply(ctx context.Context, input ReplyInput, next ReplyHandler) <-chan event.Event {
	if m.wallClock <= 0 && m.idle <= 0 {
		return next(ctx, input)
	}

	ctx2, cancel := context.WithCancel(ctx)
	if m.wallClock > 0 {
		var timeoutCancel context.CancelFunc
		ctx2, timeoutCancel = context.WithTimeout(ctx, m.wallClock)
		deferred := cancel
		cancel = func() { timeoutCancel(); deferred() }
	}

	inner := next(ctx2, input)
	out := make(chan event.Event, 16)

	go func() {
		defer close(out)
		defer cancel()

		var idleCh <-chan time.Time
		var idleTimer *time.Timer
		if m.idle > 0 {
			idleTimer = time.NewTimer(m.idle)
			defer idleTimer.Stop()
			idleCh = idleTimer.C
		}

		for {
			select {
			case evt, ok := <-inner:
				if !ok {
					return
				}
				select {
				case out <- evt:
				case <-ctx2.Done():
					drainEvents(inner)
					return
				}
				// Reset the idle timer only AFTER the event has been
				// forwarded: a slow downstream consumer is backpressure,
				// not a stalled reply (HARNESS review M1).
				if idleTimer != nil {
					if !idleTimer.Stop() {
						select {
						case <-idleTimer.C:
						default:
						}
					}
					idleTimer.Reset(m.idle)
				}
			case <-idleCh:
				// Stalled: cancel the reply and drain so the producer's
				// ctx-aware sends unblock and it can terminate.
				cancel()
				drainEvents(inner)
				return
			}
		}
	}()
	return out
}

// drainEvents consumes and discards remaining events so producers blocked
// on sends are released.
func drainEvents(ch <-chan event.Event) {
	for range ch {
	}
}
