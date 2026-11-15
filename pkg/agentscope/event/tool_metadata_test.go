package event

import (
	"reflect"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestToolResultEndMetadata(t *testing.T) {
	end := NewToolResultEndEvent("reply", "call", message.ToolResultSuccess)
	if end.GetMetadata() != nil {
		t.Fatal("new result has unexpected metadata")
	}
	end.Metadata = map[string]any{"answers": []string{"Go"}}
	if !reflect.DeepEqual(end.GetMetadata(), end.Metadata) {
		t.Fatal("metadata accessor lost the result")
	}
}
