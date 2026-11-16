package formatter

import (
	"fmt"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Only an ERROR tool result gains the Anthropic is_error flag; every other
// state (including the empty state) keeps the plain tool_result shape, and
// empty error output still uses the "(empty tool output)" placeholder
// (upstream #3905).
func TestAnthropicFormatterToolResultIsError(t *testing.T) {
	states := []message.ToolResultState{
		message.ToolResultSuccess,
		message.ToolResultError,
		message.ToolResultInterrupted,
		message.ToolResultDenied,
		message.ToolResultRunning,
		"",
	}
	for _, state := range states {
		for _, output := range []any{"failed", ""} {
			name := fmt.Sprintf("%v/%v", state, output)
			if state == "" {
				name = fmt.Sprintf("zero-state/%v", output)
			}
			t.Run(name, func(t *testing.T) {
				msg := message.AssistantMsg("bot", []message.ContentBlock{
					message.ToolCallBlock{
						Type: "tool_call", ID: "toolu_1", Name: "validate",
						Input: "{}", State: message.ToolCallFinished,
					},
					message.ToolResultBlock{
						Type: "tool_result", ID: "toolu_1", Name: "validate",
						Output: output, State: state,
					},
				})
				f := NewAnthropicFormatter()
				for _, multi := range []bool{false, true} {
					var out []map[string]any
					var err error
					if multi {
						out, err = f.FormatMultiAgent([]*message.Msg{msg}, "bot")
					} else {
						out, err = f.Format([]*message.Msg{msg})
					}
					if err != nil {
						t.Fatal(err)
					}
					if len(out) != 2 {
						t.Fatalf("multi=%v: expanded into %d wire messages, want 2: %v", multi, len(out), out)
					}
					if out[1]["role"] != "user" {
						t.Fatalf("multi=%v: tool_result message role = %v, want user", multi, out[1]["role"])
					}
					blocks := out[1]["content"].([]map[string]any)
					if len(blocks) != 1 {
						t.Fatalf("multi=%v: user content = %v", multi, blocks)
					}
					tr := blocks[0]
					if tr["type"] != "tool_result" || tr["tool_use_id"] != "toolu_1" {
						t.Fatalf("multi=%v: tool_result = %v", multi, tr)
					}
					wantContent := any(output)
					if output == "" {
						wantContent = "(empty tool output)"
					}
					if tr["content"] != wantContent {
						t.Fatalf("multi=%v: content = %v, want %v", multi, tr["content"], wantContent)
					}
					got, hasFlag := tr["is_error"]
					if state == message.ToolResultError {
						if !hasFlag || got != true {
							t.Fatalf("multi=%v: is_error = %v (present=%v), want true", multi, got, hasFlag)
						}
					} else if hasFlag {
						t.Fatalf("multi=%v: is_error = %v present for state %q, want absent", multi, got, state)
					}
				}
			})
		}
	}
}
