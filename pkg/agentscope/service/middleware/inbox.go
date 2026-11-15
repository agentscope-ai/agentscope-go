package middleware

import (
	"context"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	mw "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
)

// InboxProvider retrieves pending messages for an agent from external sources
// (e.g., team inbox, cross-session messaging).
type InboxProvider interface {
	DrainMessages(ctx context.Context, agentName string) ([]*message.Msg, error)
}

// InboxMiddleware injects pending inbox messages into the agent's context
// at the start of each reply. This enables cross-session communication
// between team members.
type InboxMiddleware struct {
	mw.BaseMiddleware
	inbox InboxProvider
}

// NewInboxMiddleware creates an InboxMiddleware.
func NewInboxMiddleware(inbox InboxProvider) *InboxMiddleware {
	return &InboxMiddleware{
		BaseMiddleware: mw.BaseMiddleware{MiddlewareKey: "inbox"},
		inbox:          inbox,
	}
}

func (m *InboxMiddleware) OnReply(
	ctx context.Context,
	input mw.ReplyInput,
	next mw.ReplyHandler,
) <-chan event.Event {
	msgs, err := m.inbox.DrainMessages(ctx, input.AgentName)
	if err == nil && len(msgs) > 0 {
		// Prepend inbox messages to the user input
		var prefix string
		for _, msg := range msgs {
			if txt := msg.GetTextContent("\n"); txt != nil {
				prefix += "[Team message from " + msg.Name + "]: " + *txt + "\n"
			}
		}
		if prefix != "" {
			input.UserInput = prefix + "\n" + input.UserInput
		}
	}
	return next(ctx, input)
}
