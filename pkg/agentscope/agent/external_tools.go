package agent

import (
	"context"
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// executeExternalTool is shared by normal execution and checkpoint resume.
// The caller checks current permissions before entering this handoff.
func (a *UnifiedAgent) executeExternalTool(ctx context.Context, ch chan<- event.Event, replyID string, tc *message.ToolCallBlock, t tool.Tool, resumed bool) toolOutcome {
	input, err := tc.ParseInput()
	if err == nil {
		err = tool.ValidateInput(t.InputSchema(), input)
	}
	if err == nil {
		if validator, ok := t.(tool.InputValidator); ok {
			err = validator.ValidateInput(input)
		}
	}
	if err != nil {
		return a.emitToolResult(ctx, ch, replyID, tc, message.ToolResultError,
			fmt.Sprintf("external tool input validation: %v", err))
	}

	a.updateToolCallState(tc.ID, message.ToolCallSubmitted)
	emit(ctx, ch, event.NewToolResultStartEvent(replyID, tc.ID, tc.Name))
	emit(ctx, ch, event.NewRequireExternalExecutionEvent(replyID, []message.ToolCallBlock{*tc}))
	ids := answerIDs{reply: replyID}
	if resumed {
		ids = a.resumedAnswerIDs(replyID)
	}
	result := a.waitForExternalResult(ctx, ids, tc.ID)
	if result == nil {
		result = &message.ToolResultBlock{State: message.ToolResultError,
			Output: "External execution timed out, canceled, or no matching result was submitted"}
	} else if result.State == message.ToolResultSuccess {
		if validator, ok := t.(tool.ExternalResultValidator); ok {
			if err := validator.ValidateExternalResult(input, result); err != nil {
				result = &message.ToolResultBlock{State: message.ToolResultError,
					Output: fmt.Sprintf("external tool result validation: %v", err)}
			}
		}
	}

	// Emit supported blocks in their submitted order. Keep the original output
	// for state persistence, including blocks with no event representation.
	if blocks, ok := result.Output.([]message.ContentBlock); ok {
		for _, block := range blocks {
			if text, ok := block.(message.TextBlock); ok {
				emit(ctx, ch, event.NewToolResultTextDeltaEvent(replyID, tc.ID, text.Text))
			} else {
				emitToolResultData(ctx, ch, replyID, tc.ID, []message.ContentBlock{block})
			}
		}
	} else {
		emit(ctx, ch, event.NewToolResultTextDeltaEvent(replyID, tc.ID, result.GetOutputText()))
	}
	end := event.NewToolResultEndEvent(replyID, tc.ID, result.State)
	end.Metadata = result.Metadata
	emit(ctx, ch, end)
	output := result.Output
	// Normal execution historically stored a single text block as a string.
	// Resumed results retain the host's original representation.
	if blocks, ok := output.([]message.ContentBlock); !resumed && ok && len(blocks) == 1 {
		if text, ok := blocks[0].(message.TextBlock); ok {
			output = text.Text
		}
	}
	return toolOutcome{state: result.State, externalOutput: output, metadata: result.Metadata}
}
