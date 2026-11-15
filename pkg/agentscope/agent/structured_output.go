package agent

import (
	"context"
	"encoding/json"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// ApplyResponseFormat applies the agent-level ResponseFormat to a model call.
// If the agent has a ResponseFormat configured, it adds WithResponseFormat to
// the call options. This is called internally by the agent before each model call.
func ApplyResponseFormat(opts []model.CallOption, rf *model.ResponseFormat) []model.CallOption {
	if rf == nil {
		return opts
	}
	return append(opts, model.WithResponseFormat(rf))
}

// GenerateStructuredResponse uses the agent model to produce a response
// conforming to the given JSON schema. It delegates to model.GenerateStructuredOutput
// using the agent state context as conversation history.
func (a *UnifiedAgent) GenerateStructuredResponse(ctx context.Context, schema json.RawMessage) (json.RawMessage, error) {
	a.mu.Lock()
	msgs := a.prepareModelInputLocked(ctx)
	a.mu.Unlock()
	return model.GenerateStructuredOutput(ctx, a.model, msgs, schema)
}

// prepareModelInputLocked is an internal helper that builds model input messages.
// Caller must hold a.mu.
func (a *UnifiedAgent) prepareModelInputLocked(ctx context.Context) []*message.Msg {
	prompt := a.systemPrompt

	if a.stateAwareness {
		prompt = InjectStateAwareness(prompt, a.state)
	}

	sysMsg := message.SystemMsg(a.name, prompt)
	msgs := make([]*message.Msg, 0, len(a.state.Context)+2)
	msgs = append(msgs, sysMsg)

	if a.state.Summary != "" {
		msgs = append(msgs, message.UserMsg(a.name, "[Previous context summary]: "+a.state.Summary))
	}

	msgs = append(msgs, a.state.Context...)
	return msgs
}
