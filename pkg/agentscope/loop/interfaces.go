// pkg/agentscope/loop/interfaces.go
package loop

import (
	"context"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// ModelCaller abstracts the model API call. Implementations wrap a
// model.ChatModel and may add middleware, retries, or caching.
type ModelCaller interface {
	Call(ctx context.Context, messages []*message.Msg,
		tools []model.ToolSchema) (*model.ChatResponse, error)
}

// ModelCallerFunc adapts a plain function to the ModelCaller interface.
type ModelCallerFunc func(ctx context.Context, messages []*message.Msg,
	tools []model.ToolSchema) (*model.ChatResponse, error)

func (f ModelCallerFunc) Call(ctx context.Context, messages []*message.Msg,
	tools []model.ToolSchema) (*model.ChatResponse, error) {
	return f(ctx, messages, tools)
}

// ToolExecutor abstracts tool execution. Implementations handle permission
// checks, sandboxing, and concurrent batching.
type ToolExecutor interface {
	Execute(ctx context.Context, call message.ToolCallBlock) (*tool.ToolResponse, error)
	BatchExecute(ctx context.Context, calls []message.ToolCallBlock) []*ToolResult
}

// ToolResult pairs a tool call with its execution result or error. It aliases
// tool.OrchestratorResult so tool.Orchestrator can implement ToolExecutor
// without introducing a tool -> loop import cycle.
type ToolResult = tool.OrchestratorResult

// ContextManager manages the conversation message history.
type ContextManager interface {
	Append(msg *message.Msg)
	Messages() []*message.Msg
	Compress(ctx context.Context) error
	TokenCount() int
}

// ToolSchemaProvider returns the set of tool schemas available for model calls.
type ToolSchemaProvider interface {
	GetToolSchemas() []model.ToolSchema
}
