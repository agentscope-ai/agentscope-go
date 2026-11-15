package middleware

import (
	"context"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event/streamcheck"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/logging"
)

// StreamValidator is an OPT-IN development middleware that runs the
// streamcheck invariants over every reply's event stream and logs
// violations (HARNESS_DESIGN B3). Production keeps it off: it buffers
// events in memory for the duration of the reply.
type StreamValidator struct {
	BaseMiddleware
	agentName string
}

// NewStreamValidator creates the validator middleware.
func NewStreamValidator(agentName string) *StreamValidator {
	return &StreamValidator{
		BaseMiddleware: BaseMiddleware{MiddlewareKey: "stream-validator"},
		agentName:      agentName,
	}
}

// OnReply buffers the stream, validating it when the reply ends.
func (v *StreamValidator) OnReply(ctx context.Context, input ReplyInput, next ReplyHandler) <-chan event.Event {
	inner := next(ctx, input)
	out := make(chan event.Event, 16)
	go func() {
		defer close(out)
		var events []event.Event
		for evt := range inner {
			events = append(events, evt)
			select {
			case out <- evt:
			case <-ctx.Done():
				return
			}
		}
		if err := streamcheck.Validate(events); err != nil {
			logging.Warn("event stream invariant violations",
				"agent", firstNonEmpty(v.agentName, input.AgentName),
				"err", err)
		}
	}()
	return out
}
