package tool

import (
	"context"
	"fmt"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

type semanticValidationTool struct {
	BaseTool
	executed bool
}

func (*semanticValidationTool) ValidateInput(map[string]any) error {
	return fmt.Errorf("question labels must be unique")
}

func (t *semanticValidationTool) Execute(context.Context, map[string]any) (*ToolResponse, error) {
	t.executed = true
	return NewTextResponse("unexpected execution"), nil
}

func TestToolkitHonorsSemanticInputValidation(t *testing.T) {
	validated := &semanticValidationTool{BaseTool: BaseTool{ToolName: "validated"}}
	resp, err := NewToolkit(validated).CallTool(context.Background(), "validated", map[string]any{})
	if err != nil || resp == nil || resp.State != message.ToolResultError || validated.executed {
		t.Fatalf("validation did not stop execution: response=%+v, err=%v, executed=%v", resp, err, validated.executed)
	}
}
