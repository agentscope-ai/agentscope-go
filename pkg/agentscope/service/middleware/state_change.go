package middleware

import (
	"context"

	mw "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// StateChangeListener is called after each tool execution to report state changes.
type StateChangeListener interface {
	OnStateChange(ctx context.Context, agentName string, toolName string, change map[string]any)
}

// StateChangeMiddleware emits state change notifications after tool executions.
// This is useful for pushing UI updates when agent state mutates.
type StateChangeMiddleware struct {
	mw.BaseMiddleware
	listener StateChangeListener
}

// NewStateChangeMiddleware creates a StateChangeMiddleware.
func NewStateChangeMiddleware(listener StateChangeListener) *StateChangeMiddleware {
	return &StateChangeMiddleware{
		BaseMiddleware: mw.BaseMiddleware{MiddlewareKey: "state_change"},
		listener:       listener,
	}
}

func (m *StateChangeMiddleware) OnActing(
	ctx context.Context,
	input *mw.ActingInput,
	next mw.ActingHandler,
) (*tool.ToolResponse, error) {
	resp, err := next(ctx, input)
	if err == nil && resp != nil && m.listener != nil {
		change := map[string]any{
			"tool":  input.ToolCall.Name,
			"state": string(resp.State),
		}
		if resp.Metadata != nil {
			change["metadata"] = resp.Metadata
		}
		m.listener.OnStateChange(ctx, input.AgentName, input.ToolCall.Name, change)
	}
	return resp, err
}

func (m *StateChangeMiddleware) OnModelCall(
	ctx context.Context,
	input *mw.ModelCallInput,
	next mw.ModelCallHandler,
) (*model.ChatResponse, error) {
	return next(ctx, input)
}
