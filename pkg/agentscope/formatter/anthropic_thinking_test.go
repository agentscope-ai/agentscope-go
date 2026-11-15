package formatter

import (
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"reflect"
	"testing"
)

func TestAnthropicFormatterKeepsEmptySignedThinking(t *testing.T) {
	msg := message.AssistantMsg("a", []message.ContentBlock{
		message.TextBlock{Type: "text"},
		message.ThinkingBlock{Type: "thinking"},
		message.ThinkingBlock{Type: "thinking", Extra: map[string]any{"signature": ""}},
		message.ThinkingBlock{Type: "thinking", Extra: map[string]any{"signature": "opaque"}},
		message.RedactedThinkingBlock{Type: "redacted_thinking", Data: "hidden"},
	})
	want := []map[string]any{{"type": "thinking", "thinking": "", "signature": "opaque"}, {"type": "redacted_thinking", "data": "hidden"}}
	got, err := NewAnthropicFormatter().Format([]*message.Msg{msg})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0]["content"], want) {
		t.Fatalf("formatted=%v want=%v", got, want)
	}
}
