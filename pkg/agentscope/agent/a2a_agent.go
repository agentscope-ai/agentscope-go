package agent

import (
	"context"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/a2a"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// A2AAgent uses an a2a.Client to forward Reply calls to a remote agent service.
type A2AAgent struct {
	AgentBase

	Name   string
	Client a2a.Client
}

func NewA2AAgent(name string, client a2a.Client) *A2AAgent {
	return &A2AAgent{
		AgentBase: NewAgentBase(),
		Name:      name,
		Client:    client,
	}
}

func (a *A2AAgent) Reply(ctx context.Context, args ...any) (*message.Msg, error) {
	var msgs []*message.Msg
	for _, arg := range args {
		if m, ok := arg.(*message.Msg); ok {
			msgs = append(msgs, m)
		}
	}
	return a.Client.Call(ctx, msgs)
}

func (a *A2AAgent) Observe(ctx context.Context, msgs []*message.Msg) error {
	_ = ctx
	_ = msgs
	return nil
}
