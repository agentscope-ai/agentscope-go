package loop

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func TestOrchestratorImplementsToolExecutor(t *testing.T) {
	o := tool.NewOrchestrator(tool.OrchestratorConfig{Toolkit: tool.NewToolkit()})
	var executor ToolExecutor = o.AsToolExecutor()

	results := executor.BatchExecute(context.Background(), []message.ToolCallBlock{{Name: "missing", Input: `{}`}})
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("expected one missing-tool error result, got %#v", results)
	}
}
