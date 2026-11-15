package middleware

import (
	"context"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// BuildReplyChain wraps a core ReplyHandler with middlewares in onion order.
// Execution order: middlewares[0] is outermost (first to enter, last to exit).
func BuildReplyChain(middlewares []Middleware, core ReplyHandler) ReplyHandler {
	handler := core
	for i := len(middlewares) - 1; i >= 0; i-- {
		mw := middlewares[i]
		next := handler
		handler = func(ctx context.Context, input ReplyInput) <-chan event.Event {
			return mw.OnReply(ctx, input, next)
		}
	}
	return handler
}

// BuildReasoningChain wraps a core ReasoningHandler with middlewares in onion order.
func BuildReasoningChain(middlewares []Middleware, core ReasoningHandler) ReasoningHandler {
	handler := core
	for i := len(middlewares) - 1; i >= 0; i-- {
		mw := middlewares[i]
		next := handler
		handler = func(ctx context.Context, input ReasoningInput) <-chan event.Event {
			return mw.OnReasoning(ctx, input, next)
		}
	}
	return handler
}

// BuildModelCallChain wraps a core ModelCallHandler with middlewares in onion order.
func BuildModelCallChain(middlewares []Middleware, core ModelCallHandler) ModelCallHandler {
	handler := core
	for i := len(middlewares) - 1; i >= 0; i-- {
		mw := middlewares[i]
		next := handler
		handler = func(ctx context.Context, input *ModelCallInput) (*model.ChatResponse, error) {
			return mw.OnModelCall(ctx, input, next)
		}
	}
	return handler
}

// BuildActingChain wraps a core ActingHandler with middlewares in onion order.
func BuildActingChain(middlewares []Middleware, core ActingHandler) ActingHandler {
	handler := core
	for i := len(middlewares) - 1; i >= 0; i-- {
		mw := middlewares[i]
		next := handler
		handler = func(ctx context.Context, input *ActingInput) (*tool.ToolResponse, error) {
			return mw.OnActing(ctx, input, next)
		}
	}
	return handler
}

// BuildCompressChain wraps a core CompressHandler with middlewares in onion order.
func BuildCompressChain(middlewares []Middleware, core CompressHandler) CompressHandler {
	handler := core
	for i := len(middlewares) - 1; i >= 0; i-- {
		mw := middlewares[i]
		next := handler
		handler = func(ctx context.Context, input CompressInput) error {
			return mw.OnCompressContext(ctx, input, next)
		}
	}
	return handler
}

// BuildCheckPermissionChain wraps a core CheckPermissionHandler with middlewares in onion order.
func BuildCheckPermissionChain(middlewares []Middleware, core CheckPermissionHandler) CheckPermissionHandler {
	handler := core
	for i := len(middlewares) - 1; i >= 0; i-- {
		mw := middlewares[i]
		next := handler
		handler = func(ctx context.Context, input *CheckPermissionInput) (*permission.Decision, error) {
			return mw.OnCheckPermission(ctx, input, next)
		}
	}
	return handler
}

// ApplySystemPromptPipeline runs OnSystemPrompt through all middlewares sequentially.
// Unlike the onion hooks, this is a simple pipeline: each middleware transforms
// the prompt string and passes the result to the next.
func ApplySystemPromptPipeline(ctx context.Context, middlewares []Middleware, agentName, prompt string) string {
	for _, mw := range middlewares {
		prompt = mw.OnSystemPrompt(ctx, agentName, prompt)
	}
	return prompt
}
