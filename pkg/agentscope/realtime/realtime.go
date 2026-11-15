package realtime

import (
	"context"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Stream is the minimal streaming interface, used as a placeholder.
type Stream interface {
	Recv() (*message.Msg, error)
	Close() error
}

// Client abstracts a realtime model interface, for example a WebSocket-based streaming API.
type Client interface {
	// Start begins a new streaming session using the initial messages.
	Start(ctx context.Context, initMsgs []*message.Msg) (Stream, error)
}
