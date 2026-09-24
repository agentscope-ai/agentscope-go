package agent

import (
	"context"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
)

// Agent/session labels come from the host-owned agent; tenant authorization
// remains the caller's responsibility. Never derive it from user input.
func (a *UnifiedAgent) inferenceContext(ctx context.Context) context.Context {
	id := inference.IdentityFromContext(ctx)
	a.mu.Lock()
	id.AgentName = a.name
	id.SessionID = a.state.SessionID
	a.mu.Unlock()
	return inference.WithIdentity(ctx, id)
}
