package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// UnifiedAgentRunner adapts a UnifiedAgent for use with runtime.SessionEngine.
// It creates loop.Option values that wire the agent's model and toolkit into
// a loop.Loop configuration.
type UnifiedAgentRunner struct {
	agent     *UnifiedAgent
	loopHooks []loop.Hook
}

// NewUnifiedAgentRunner creates a runner that bridges a UnifiedAgent to loop.Loop.
func NewUnifiedAgentRunner(agent *UnifiedAgent, hooks ...loop.Hook) *UnifiedAgentRunner {
	return &UnifiedAgentRunner{
		agent:     agent,
		loopHooks: hooks,
	}
}

// LoopOptions returns loop.Option values for configuring a loop.Loop that
// delegates model calls and tool execution to the underlying UnifiedAgent.
func (r *UnifiedAgentRunner) LoopOptions() []loop.Option {
	executor := &toolExecutorAdapter{
		agent: r.agent,
		orchestrator: tool.NewOrchestrator(tool.OrchestratorConfig{
			Toolkit:    r.agent.toolkit,
			PermEngine: r.agent.engine,
		}),
	}
	opts := []loop.Option{
		loop.WithModelCaller(&modelCallerAdapter{agent: r.agent}),
		loop.WithToolExecutor(executor),
		loop.WithSchemaProvider(r.agent.toolkit),
		loop.WithMaxIters(r.agent.reactCfg.MaxIters),
		loop.WithSystemPrompt(r.agent.systemPrompt),
	}
	if len(r.loopHooks) > 0 {
		opts = append(opts, loop.WithHooks(r.loopHooks...))
	}
	return opts
}

// modelCallerAdapter implements loop.ModelCaller by delegating to the
// agent's callModel method, which handles retries and fallback.
type modelCallerAdapter struct {
	agent *UnifiedAgent
}

func (m *modelCallerAdapter) Call(ctx context.Context, msgs []*message.Msg, tools []model.ToolSchema) (*model.ChatResponse, error) {
	var opts []model.CallOption
	if len(tools) > 0 {
		opts = append(opts, model.WithTools(tools))
	}
	return m.agent.callModel(ctx, msgs, opts)
}

// toolExecutorAdapter implements loop.ToolExecutor through the same permission
// and acting-middleware layers used by UnifiedAgent. The loop protocol has no
// HITL event channel, so confirmation-required and external tools fail closed;
// callers that need interactive approval must use UnifiedAgent.ReplyStream.
// WithExclusiveTools is honored per batch. The loop has
// no handed-off end: MarkHandedOff does not stop a loop run.
type toolExecutorAdapter struct {
	agent        *UnifiedAgent
	orchestrator *tool.Orchestrator
}

func (t *toolExecutorAdapter) Execute(ctx context.Context, call message.ToolCallBlock) (*tool.ToolResponse, error) { //nolint:gocritic // interface
	core := func(ctx context.Context, input *middleware.ActingInput) (*tool.ToolResponse, error) {
		resolved := t.agent.toolkit.Get(input.ToolCall.Name)
		if resolved != nil && resolved.IsExternalTool() {
			return tool.NewErrorResponse(fmt.Errorf("tool %q requires external execution; use UnifiedAgent.ReplyStream", input.ToolCall.Name)), nil
		}
		return t.orchestrator.Execute(ctx, input.ToolCall)
	}
	handler := core
	if len(t.agent.middlewares) > 0 {
		handler = middleware.BuildActingChain(t.agent.middlewares, core)
	}
	return handler(ctx, &middleware.ActingInput{AgentName: t.agent.name, ToolCall: call})
}

func (t *toolExecutorAdapter) BatchExecute(ctx context.Context, calls []message.ToolCallBlock) []*loop.ToolResult {
	var results []*loop.ToolResult
	if first := t.agent.exclusiveViolation(calls); first != "" {
		for i := range calls {
			reject := tool.NewErrorResponse(errors.New(t.agent.exclusiveRejection(&calls[i], first)))
			results = append(results, &loop.ToolResult{Call: calls[i], Response: reject})
		}
		return results
	}
	for _, c := range calls {
		resp, err := t.Execute(ctx, c)
		results = append(results, &loop.ToolResult{Call: c, Response: resp, Err: err})
	}
	return results
}
