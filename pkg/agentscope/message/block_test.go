package message

import (
	"encoding/json"
	"testing"
)

func TestRedactedThinkingBlock_RoundTrip(t *testing.T) {
	original := RedactedThinkingBlock{
		Type: "redacted_thinking",
		ID:   "rt-001",
		Data: "opaque-redacted-data-abc123",
	}

	// Marshal
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Unmarshal into a new block
	var decoded RedactedThinkingBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Type != original.Type {
		t.Errorf("Type: got %q, want %q", decoded.Type, original.Type)
	}
	if decoded.ID != original.ID {
		t.Errorf("ID: got %q, want %q", decoded.ID, original.ID)
	}
	if decoded.Data != original.Data {
		t.Errorf("Data: got %q, want %q", decoded.Data, original.Data)
	}
}

func TestRedactedThinkingBlock_GetType(t *testing.T) {
	b := RedactedThinkingBlock{Type: "redacted_thinking", ID: "rt-002"}
	if b.GetType() != ContentBlockRedactedThinking {
		t.Errorf("GetType() = %v, want %v", b.GetType(), ContentBlockRedactedThinking)
	}
}

func TestRedactedThinkingBlock_GetID(t *testing.T) {
	b := RedactedThinkingBlock{Type: "redacted_thinking", ID: "rt-003"}
	if b.GetID() != "rt-003" {
		t.Errorf("GetID() = %q, want %q", b.GetID(), "rt-003")
	}
}

func TestRedactedThinkingBlock_EmptyData(t *testing.T) {
	b := RedactedThinkingBlock{
		Type: "redacted_thinking",
		ID:   "rt-004",
	}

	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded RedactedThinkingBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Data != "" {
		t.Errorf("expected empty Data, got %q", decoded.Data)
	}
}

func TestRedactedThinkingBlock_UnmarshalContentBlocks(t *testing.T) {
	// Test round-trip through the full UnmarshalContentBlocks pathway.
	blocks := []ContentBlock{
		ThinkingBlock{Type: "thinking", ID: "th-1", Thinking: "some reasoning"},
		RedactedThinkingBlock{Type: "redacted_thinking", ID: "rt-5", Data: "secret"},
		TextBlock{Type: "text", ID: "t-1", Text: "final answer"},
	}

	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, err := UnmarshalContentBlocks(raw)
	if err != nil {
		t.Fatalf("UnmarshalContentBlocks: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(got))
	}

	// Verify thinking block
	tb, ok := got[0].(ThinkingBlock)
	if !ok {
		t.Fatalf("block 0: expected ThinkingBlock, got %T", got[0])
	}
	if tb.Thinking != "some reasoning" {
		t.Errorf("thinking text = %q, want %q", tb.Thinking, "some reasoning")
	}

	// Verify redacted thinking block
	rtb, ok := got[1].(RedactedThinkingBlock)
	if !ok {
		t.Fatalf("block 1: expected RedactedThinkingBlock, got %T", got[1])
	}
	if rtb.Data != "secret" {
		t.Errorf("redacted data = %q, want %q", rtb.Data, "secret")
	}
	if rtb.GetType() != ContentBlockRedactedThinking {
		t.Errorf("redacted GetType() = %v, want %v", rtb.GetType(), ContentBlockRedactedThinking)
	}

	// Verify text block
	txtb, ok := got[2].(TextBlock)
	if !ok {
		t.Fatalf("block 2: expected TextBlock, got %T", got[2])
	}
	if txtb.Text != "final answer" {
		t.Errorf("text = %q, want %q", txtb.Text, "final answer")
	}
}
