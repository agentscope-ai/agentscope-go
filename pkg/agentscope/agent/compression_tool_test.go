package agent

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// Upstream #2143: WithAgentDrivenCompression registers the compress_context
// tool, and it survives a WithToolkit that appears earlier in the options.
func TestWithAgentDrivenCompressionRegistersTool(t *testing.T) {
	a := NewUnifiedAgent("t", "sys", imgTestModel{},
		WithToolkit(tool.NewToolkit()),
		WithAgentDrivenCompression(),
	)
	if a.toolkit.Get("compress_context") == nil {
		t.Fatal("compress_context tool not registered")
	}
	// Calling it is safe even without a context config (no-op compression).
	resp, err := a.toolkit.CallTool(context.Background(), "compress_context", map[string]any{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.State == message.ToolResultError {
		t.Errorf("unexpected error state: %v", resp.Content)
	}
}

func TestWithoutAgentDrivenCompressionNoTool(t *testing.T) {
	a := NewUnifiedAgent("t", "sys", imgTestModel{}, WithToolkit(tool.NewToolkit()))
	if a.toolkit.Get("compress_context") != nil {
		t.Error("compress_context must not be registered by default")
	}
}
