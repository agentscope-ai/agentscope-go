package agenttest

import (
	"context"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// ScriptedModelCaller returns pre-configured responses in order.
// After the script is exhausted it returns a "(script exhausted)" text response.
type ScriptedModelCaller struct {
	mu        sync.Mutex
	responses []*model.ChatResponse
	callCount int
}

// NewScriptedModelCaller creates a ModelCaller that replays the given
// responses one by one on each Call invocation.
func NewScriptedModelCaller(responses ...*model.ChatResponse) *ScriptedModelCaller {
	return &ScriptedModelCaller{responses: responses}
}

// Call returns the next scripted response, or a fallback after exhaustion.
func (m *ScriptedModelCaller) Call(_ context.Context, _ []*message.Msg, _ []model.ToolSchema) (*model.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := m.callCount
	m.callCount++
	if idx < len(m.responses) {
		return m.responses[idx], nil
	}
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "(script exhausted)"}},
		IsLast:  true,
	}, nil
}

// CallCount returns how many times Call has been invoked.
func (m *ScriptedModelCaller) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callCount
}

// SimpleToolExecutor returns a fixed text output for every tool execution.
type SimpleToolExecutor struct {
	mu        sync.Mutex
	output    string
	callCount int
}

// NewSimpleToolExecutor creates a ToolExecutor that always returns the given output text.
func NewSimpleToolExecutor(output string) *SimpleToolExecutor {
	return &SimpleToolExecutor{output: output}
}

// Execute returns a successful ToolResponse containing the configured output text.
func (e *SimpleToolExecutor) Execute(_ context.Context, _ message.ToolCallBlock) (*tool.ToolResponse, error) { //nolint:gocritic // interface
	e.mu.Lock()
	defer e.mu.Unlock()
	e.callCount++
	return &tool.ToolResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: e.output}},
		State:   message.ToolResultSuccess,
	}, nil
}

// BatchExecute executes each call sequentially using Execute.
func (e *SimpleToolExecutor) BatchExecute(ctx context.Context, calls []message.ToolCallBlock) []*loop.ToolResult {
	var results []*loop.ToolResult
	for _, c := range calls {
		resp, err := e.Execute(ctx, c)
		results = append(results, &loop.ToolResult{Call: c, Response: resp, Err: err})
	}
	return results
}

// CallCount returns how many times Execute has been invoked.
func (e *SimpleToolExecutor) CallCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.callCount
}
