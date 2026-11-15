package loop

import (
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestIsThinkingOnlyResponse_OnlyThinking(t *testing.T) {
	content := []message.ContentBlock{
		message.ThinkingBlock{Type: "thinking", ID: "th-1", Thinking: "Let me reason about this..."},
	}
	if !isThinkingOnlyResponse(content) {
		t.Error("expected true for content with only thinking blocks")
	}
}

func TestIsThinkingOnlyResponse_MultipleThinking(t *testing.T) {
	content := []message.ContentBlock{
		message.ThinkingBlock{Type: "thinking", ID: "th-1", Thinking: "First thought"},
		message.ThinkingBlock{Type: "thinking", ID: "th-2", Thinking: "Second thought"},
	}
	if !isThinkingOnlyResponse(content) {
		t.Error("expected true for content with multiple thinking blocks")
	}
}

func TestIsThinkingOnlyResponse_ThinkingAndText(t *testing.T) {
	content := []message.ContentBlock{
		message.ThinkingBlock{Type: "thinking", ID: "th-1", Thinking: "reasoning"},
		message.TextBlock{Type: "text", ID: "t-1", Text: "Here is my answer."},
	}
	if isThinkingOnlyResponse(content) {
		t.Error("expected false when text block is present")
	}
}

func TestIsThinkingOnlyResponse_ThinkingAndToolCall(t *testing.T) {
	content := []message.ContentBlock{
		message.ThinkingBlock{Type: "thinking", ID: "th-1", Thinking: "I need to run bash"},
		message.ToolCallBlock{
			Type: "tool_use",
			ID:   "call-1",
			Name: "Bash",
		},
	}
	if isThinkingOnlyResponse(content) {
		t.Error("expected false when tool call block is present")
	}
}

func TestIsThinkingOnlyResponse_EmptyContent(t *testing.T) {
	content := []message.ContentBlock{}
	if isThinkingOnlyResponse(content) {
		t.Error("expected false for empty content (no thinking blocks)")
	}
}

func TestIsThinkingOnlyResponse_NilContent(t *testing.T) {
	if isThinkingOnlyResponse(nil) {
		t.Error("expected false for nil content")
	}
}

func TestIsThinkingOnlyResponse_OnlyEmptyText(t *testing.T) {
	// Empty text blocks should not count as "real" text content.
	content := []message.ContentBlock{
		message.ThinkingBlock{Type: "thinking", ID: "th-1", Thinking: "hmm"},
		message.TextBlock{Type: "text", ID: "t-1", Text: ""},
	}
	if !isThinkingOnlyResponse(content) {
		t.Error("expected true: empty text block should not count as real content")
	}
}

func TestIsThinkingOnlyResponse_OnlyTextNoThinking(t *testing.T) {
	content := []message.ContentBlock{
		message.TextBlock{Type: "text", ID: "t-1", Text: "Just text, no thinking"},
	}
	if isThinkingOnlyResponse(content) {
		t.Error("expected false: text-only is not thinking-only")
	}
}

func TestIsThinkingOnlyResponse_OnlyToolCall(t *testing.T) {
	content := []message.ContentBlock{
		message.ToolCallBlock{Type: "tool_use", ID: "c1", Name: "Read"},
	}
	if isThinkingOnlyResponse(content) {
		t.Error("expected false: tool-call-only is not thinking-only")
	}
}
