package a2a

import (
	"context"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Client abstracts a remote agent invocation interface.
type Client interface {
	Call(ctx context.Context, msgs []*message.Msg) (*message.Msg, error)
}

// LocalClient is a placeholder implementation that directly calls a local function, useful for tests.
type LocalClient struct {
	Handler func(ctx context.Context, msgs []*message.Msg) (*message.Msg, error)
}

func (c LocalClient) Call(ctx context.Context, msgs []*message.Msg) (*message.Msg, error) {
	if c.Handler == nil {
		return nil, nil
	}
	return c.Handler(ctx, msgs)
}
