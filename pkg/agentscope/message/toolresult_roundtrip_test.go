package message

import (
	"encoding/json"
	"testing"
)

// TestToolResultBlock_StructuredOutputRoundTrips proves a ToolResultBlock whose
// Output is a []ContentBlock (e.g. a tool returning text + an image) survives a
// JSON marshal/unmarshal cycle as structured blocks, rather than degrading into
// a raw JSON string.
func TestToolResultBlock_StructuredOutputRoundTrips(t *testing.T) {
	original := []ContentBlock{
		ToolResultBlock{
			Type: "tool_result",
			ID:   "call_1",
			Name: "read_file",
			Output: []ContentBlock{
				TextBlock{Type: "text", Text: "line one"},
				TextBlock{Type: "text", Text: "line two"},
			},
			State: ToolResultSuccess,
		},
	}

	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, err := UnmarshalContentBlocks(raw)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 block, got %d", len(got))
	}

	trb, ok := got[0].(ToolResultBlock)
	if !ok {
		t.Fatalf("expected ToolResultBlock, got %T", got[0])
	}

	blocks, ok := trb.Output.([]ContentBlock)
	if !ok {
		t.Fatalf("Output degraded to %T (%v); expected []ContentBlock", trb.Output, trb.Output)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 inner blocks, got %d", len(blocks))
	}
	if tb, ok := blocks[0].(TextBlock); !ok || tb.Text != "line one" {
		t.Fatalf("inner block 0 = %#v; want TextBlock{Text:\"line one\"}", blocks[0])
	}
}

// TestToolResultBlock_StringOutputRoundTrips guards the common case: a plain
// string tool result stays a string.
func TestToolResultBlock_StringOutputRoundTrips(t *testing.T) {
	raw, err := json.Marshal([]ContentBlock{
		ToolResultBlock{Type: "tool_result", ID: "c", Name: "bash", Output: "ok\n", State: ToolResultSuccess},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := UnmarshalContentBlocks(raw)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	trb := got[0].(ToolResultBlock)
	if s, ok := trb.Output.(string); !ok || s != "ok\n" {
		t.Fatalf("Output = %#v; want string \"ok\\n\"", trb.Output)
	}
}
