package middleware

import (
	"context"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	mw "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// ToolOffloadMiddleware asynchronously executes long-running tools.
// It immediately returns a "running" status while the tool runs in the
// background. A callback is invoked when the tool completes.
type ToolOffloadMiddleware struct {
	mw.BaseMiddleware
	shouldOffload func(toolName string) bool
	onComplete    func(agentName, toolName, toolCallID string, resp *tool.ToolResponse)
	wg            sync.WaitGroup
}

// NewToolOffloadMiddleware creates a ToolOffloadMiddleware.
// shouldOffload decides which tools run in the background.
// onComplete is called when a background tool finishes.
func NewToolOffloadMiddleware(
	shouldOffload func(toolName string) bool,
	onComplete func(agentName, toolName, toolCallID string, resp *tool.ToolResponse),
) *ToolOffloadMiddleware {
	return &ToolOffloadMiddleware{
		BaseMiddleware: mw.BaseMiddleware{MiddlewareKey: "tool_offload"},
		shouldOffload:  shouldOffload,
		onComplete:     onComplete,
	}
}

func (m *ToolOffloadMiddleware) OnActing(
	ctx context.Context,
	input *mw.ActingInput,
	next mw.ActingHandler,
) (*tool.ToolResponse, error) {
	if m.shouldOffload == nil || !m.shouldOffload(input.ToolCall.Name) {
		return next(ctx, input)
	}

	// Execute in background
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		resp, err := next(context.Background(), input)
		if err != nil {
			logrus.WithError(err).WithField("tool", input.ToolCall.Name).Error("offloaded tool failed")
			resp = tool.NewErrorResponse(err)
		}
		if m.onComplete != nil {
			m.onComplete(input.AgentName, input.ToolCall.Name, input.ToolCall.ID, resp)
		}
	}()

	return &tool.ToolResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "Tool is running in the background."}},
		State:   message.ToolResultRunning,
	}, nil
}

// Wait blocks until all offloaded tools complete.
func (m *ToolOffloadMiddleware) Wait() {
	m.wg.Wait()
}
