package message_test

import (
	"reflect"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestAppendEventPreservesExternalResultMetadata(t *testing.T) {
	m := &message.Msg{ID: "reply", Role: message.RoleAssistant}
	m.AppendEvent(event.NewToolResultStartEvent("reply", "ask", "AskUser"))
	end := event.NewToolResultEndEvent("reply", "ask", message.ToolResultSuccess)
	end.Metadata = map[string]any{"answers": []any{map[string]any{"question": "Proceed?", "selected": []string{"Yes"}}}}
	m.AppendEvent(end)
	blocks := m.GetContentBlocks(message.ContentBlockToolResult)
	if len(blocks) != 1 {
		t.Fatalf("results = %v", blocks)
	}
	result := blocks[0].(message.ToolResultBlock)
	if !reflect.DeepEqual(result.Metadata, end.Metadata) {
		t.Fatalf("metadata lost: got %#v, want %#v", result.Metadata, end.Metadata)
	}
}
