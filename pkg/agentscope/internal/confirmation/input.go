// Package confirmation binds an agent's confirmation input to one reply context.
package confirmation

import (
	"context"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
)

type key struct{}
type source struct {
	owner any
	input <-chan event.UserConfirmResultEvent
}

// WithInput overrides confirmation input for this owner only. Nested agents that
// inherit the context must not consume their parent's confirmation messages.
func WithInput[T comparable](ctx context.Context, owner T, input <-chan event.UserConfirmResultEvent) context.Context {
	return context.WithValue(ctx, key{}, source{owner, input})
}

// Input returns the reply-local input, or nil for a different/unconfigured owner.
// Senders retire the source with context cancellation rather than closing it.
func Input[T comparable](ctx context.Context, owner T) <-chan event.UserConfirmResultEvent {
	s, ok := ctx.Value(key{}).(source)
	if !ok {
		return nil
	}
	stored, ok := s.owner.(T)
	if !ok || stored != owner {
		return nil
	}
	return s.input
}
