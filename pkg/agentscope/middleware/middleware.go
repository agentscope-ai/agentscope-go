package middleware

import (
	"context"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// ReplyHandler processes a reply request and returns an event stream.
type ReplyHandler func(ctx context.Context, input ReplyInput) <-chan event.Event

// ModelCallHandler calls the model and returns a response.
type ModelCallHandler func(ctx context.Context, input *ModelCallInput) (*model.ChatResponse, error)

// ActingHandler executes a tool call and returns the result.
type ActingHandler func(ctx context.Context, input *ActingInput) (*tool.ToolResponse, error)

// CompressHandler performs context compression.
type CompressHandler func(ctx context.Context, input CompressInput) error

// CheckPermissionHandler checks whether a tool call is permitted.
type CheckPermissionHandler func(ctx context.Context, input *CheckPermissionInput) (*permission.Decision, error)

// ReplyInput is passed to OnReply hooks.
type ReplyInput struct {
	AgentName string
	UserInput string
	Messages  []*message.Msg
}

// ModelCallInput is passed to OnModelCall hooks.
type ModelCallInput struct {
	AgentName  string
	Messages   []*message.Msg
	Tools      []model.ToolSchema
	ToolChoice *model.ToolChoice

	// Optional fields populated by the agent for observability middleware.
	ModelName    string   // model name (e.g. "gpt-4.1", "claude-sonnet-4-20250514")
	ProviderName string   // provider system (e.g. "openai", "anthropic", "dashscope")
	MaxTokens    *int     // max_tokens if set on the call
	Temperature  *float64 // temperature if set on the call
}

// ActingInput is passed to OnActing hooks.
type ActingInput struct {
	AgentName string
	ToolCall  message.ToolCallBlock
}

// CompressInput is passed to OnCompressContext hooks.
type CompressInput struct {
	AgentName    string
	TriggerRatio float64
	ReserveRatio float64
}

// CheckPermissionInput is passed to OnCheckPermission hooks. In the UnifiedAgent
// integration it is a detached, read-only JSON snapshot; invocation rewrites
// are rejected. See docs/middleware.md for execution-path coverage.
type CheckPermissionInput struct {
	AgentName string
	ToolCall  message.ToolCallBlock
	ToolInput map[string]any
}

// ReasoningHandler processes a reasoning step and returns an event stream.
type ReasoningHandler func(ctx context.Context, input ReasoningInput) <-chan event.Event

// ReasoningInput is passed to OnReasoning hooks.
type ReasoningInput struct {
	AgentName string
	Messages  []*message.Msg
	Iteration int
}

// Middleware defines the hooks that can intercept agent behavior.
// Implement only the hooks you need; embed BaseMiddleware for pass-through defaults.
//
// Reply-lifecycle contract: an OnReply hook may swallow a completed reply's
// ReplyEndEvent (receive it without forwarding it) to force another
// reasoning-acting round; interrupted ends cannot be swallowed. Middleware
// MUST forward CustomEvent values whose name starts with "agentscope." —
// they are internal round-boundary sentinels, and dropping them stalls
// swallow detection until the reply context ends.
type Middleware interface {
	// OnReply wraps the entire reply lifecycle (outermost hook).
	OnReply(ctx context.Context, input ReplyInput, next ReplyHandler) <-chan event.Event

	// OnReasoning wraps each reasoning step within the ReAct loop.
	OnReasoning(ctx context.Context, input ReasoningInput, next ReasoningHandler) <-chan event.Event

	// OnModelCall wraps each model API call within the ReAct loop.
	OnModelCall(ctx context.Context, input *ModelCallInput, next ModelCallHandler) (*model.ChatResponse, error)

	// OnActing wraps each tool execution.
	OnActing(ctx context.Context, input *ActingInput, next ActingHandler) (*tool.ToolResponse, error)

	// OnSystemPrompt transforms the system prompt (pipeline mode, not onion).
	// Each middleware receives the output of the previous one.
	OnSystemPrompt(ctx context.Context, agentName string, currentPrompt string) string

	// OnCompressContext wraps context compression.
	OnCompressContext(ctx context.Context, input CompressInput, next CompressHandler) error

	// OnCheckPermission wraps eligible fresh sequential interactive UnifiedAgent
	// permission checks when an engine is configured. Other execution paths do
	// not invoke it automatically; see docs/middleware.md. Middleware is trusted
	// host policy and may short-circuit or replace a decision.
	OnCheckPermission(ctx context.Context, input *CheckPermissionInput, next CheckPermissionHandler) (*permission.Decision, error)

	// ListTools returns additional tools provided by this middleware.
	// Returns nil if the middleware does not provide tools.
	ListTools() []tool.Tool

	// Key returns a unique identifier for this middleware instance.
	// Used to namespace middleware state in MiddleContext.
	Key() string
}

// BaseMiddleware provides pass-through implementations for all hooks.
// Embed this in concrete middleware structs and override only the hooks you need.
type BaseMiddleware struct {
	MiddlewareKey string
}

func (b *BaseMiddleware) Key() string { return b.MiddlewareKey }

func (b *BaseMiddleware) OnReply(ctx context.Context, input ReplyInput, next ReplyHandler) <-chan event.Event {
	return next(ctx, input)
}

func (b *BaseMiddleware) OnReasoning(ctx context.Context, input ReasoningInput, next ReasoningHandler) <-chan event.Event {
	return next(ctx, input)
}

func (b *BaseMiddleware) ListTools() []tool.Tool {
	return nil
}

func (b *BaseMiddleware) OnModelCall(ctx context.Context, input *ModelCallInput, next ModelCallHandler) (*model.ChatResponse, error) {
	return next(ctx, input)
}

func (b *BaseMiddleware) OnActing(ctx context.Context, input *ActingInput, next ActingHandler) (*tool.ToolResponse, error) {
	return next(ctx, input)
}

func (b *BaseMiddleware) OnSystemPrompt(_ context.Context, _ string, currentPrompt string) string {
	return currentPrompt
}

func (b *BaseMiddleware) OnCompressContext(ctx context.Context, input CompressInput, next CompressHandler) error {
	return next(ctx, input)
}

func (b *BaseMiddleware) OnCheckPermission(ctx context.Context, input *CheckPermissionInput, next CheckPermissionHandler) (*permission.Decision, error) {
	return next(ctx, input)
}
