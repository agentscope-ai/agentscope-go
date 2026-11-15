package formatter

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// A merged agent message (thinking, text, tool_use and tool_result blocks
// interleaved — how the unified agent stores a tool round) must expand into
// ordered wire messages where every tool_result lives in a USER message:
// the Anthropic Messages API requires tool_result blocks in user messages.
func TestAnthropicFormatterSplitsToolResultsIntoUserMessages(t *testing.T) {
	msg := message.AssistantMsg("bot", []message.ContentBlock{
		message.ThinkingBlock{Type: "thinking", ID: "th1", Thinking: "inspect first"},
		message.TextBlock{Type: "text", ID: "tx1", Text: "running git status"},
		message.ToolCallBlock{
			Type: "tool_call", ID: "toolu_1", Name: "Bash",
			Input: `{"command":"git status"}`, State: message.ToolCallPending,
		},
		message.ToolResultBlock{
			Type: "tool_result", ID: "toolu_1", Name: "Bash",
			Output: "on branch main", State: message.ToolResultSuccess,
		},
		message.TextBlock{Type: "text", ID: "tx2", Text: "now the log"},
		message.ToolCallBlock{
			Type: "tool_call", ID: "toolu_2", Name: "Bash",
			Input: `{"command":"git log"}`, State: message.ToolCallPending,
		},
		message.ToolResultBlock{
			Type: "tool_result", ID: "toolu_2", Name: "Bash",
			Output: "3 commits", State: message.ToolResultSuccess,
		},
	})

	f := NewAnthropicFormatter()
	out, err := f.Format([]*message.Msg{msg})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		b, _ := json.Marshal(out)
		t.Fatalf("expanded into %d wire messages, want 4: %s", len(out), b)
	}

	assertRole := func(i int, want string) {
		t.Helper()
		if out[i]["role"] != want {
			b, _ := json.Marshal(out[i])
			t.Fatalf("wire message %d role = %v, want %q: %s", i, out[i]["role"], want, b)
		}
	}
	assertRole(0, "assistant")
	assertRole(1, "user")
	assertRole(2, "assistant")
	assertRole(3, "user")

	blocks := func(i int) []map[string]any {
		t.Helper()
		c, _ := out[i]["content"].([]map[string]any)
		return c
	}
	// First assistant message: thinking + text + tool_use, no tool_result.
	for _, b := range blocks(0) {
		if b["type"] == "tool_result" {
			t.Fatalf("tool_result in assistant message: %v", b)
		}
	}
	// First user message: exactly the tool_result for toolu_1.
	tr := blocks(1)
	if len(tr) != 1 || tr[0]["type"] != "tool_result" || tr[0]["tool_use_id"] != "toolu_1" {
		b, _ := json.Marshal(tr)
		t.Fatalf("first user message = %s, want single tool_result toolu_1", b)
	}
	if tr[0]["content"] != "on branch main" {
		t.Fatalf("tool_result content = %v", tr[0]["content"])
	}
	// Second assistant message: text + tool_use(2); second user: toolu_2.
	for _, b := range blocks(2) {
		if b["type"] == "tool_result" {
			t.Fatalf("tool_result in second assistant message: %v", b)
		}
	}
	tr2 := blocks(3)
	if len(tr2) != 1 || tr2[0]["tool_use_id"] != "toolu_2" {
		b, _ := json.Marshal(tr2)
		t.Fatalf("second user message = %s, want single tool_result toolu_2", b)
	}
}

// A message without tool results formats exactly as before (one wire message).
func TestAnthropicFormatterPlainMessageUnchanged(t *testing.T) {
	msg := message.AssistantMsg("bot", []message.ContentBlock{
		message.TextBlock{Type: "text", ID: "tx1", Text: "hello"},
	})
	f := NewAnthropicFormatter()
	out, err := f.Format([]*message.Msg{msg})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0]["role"] != "assistant" {
		t.Fatalf("plain assistant message = %v", out)
	}
	c, _ := out[0]["content"].([]map[string]any)
	if len(c) != 1 || c[0]["type"] != "text" || c[0]["text"] != "hello" {
		t.Fatalf("content = %v", c)
	}
}

func TestAnthropicFormatterParallelResultsAndAttribution(t *testing.T) {
	for _, role := range []message.Role{message.RoleAssistant, message.RoleUser, message.Role("tool")} {
		t.Run(string(role), func(t *testing.T) {
			msg := message.NewMsg("other", role, []message.ContentBlock{
				message.ToolResultBlock{Type: "tool_result", ID: "a", Output: "one"},
				message.ToolResultBlock{Type: "tool_result", ID: "b", Output: "two"},
				message.TextBlock{Type: "text", Text: "after"},
			})
			before, _ := json.Marshal(msg)
			f := NewAnthropicFormatter()
			for _, multi := range []bool{false, true} {
				var out []map[string]any
				var err error
				if multi {
					out, err = f.FormatMultiAgent([]*message.Msg{nil, msg}, "self")
				} else {
					out, err = f.Format([]*message.Msg{nil, msg})
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(out) != 2 {
					t.Fatalf("messages=%v", out)
				}
				results := out[0]["content"].([]map[string]any)
				if out[0]["role"] != "user" || len(results) != 2 || results[0]["tool_use_id"] != "a" || results[1]["tool_use_id"] != "b" {
					t.Fatalf("results=%v", out[0])
				}
				after, _ := json.Marshal(msg)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("formatter mutated history")
				}
				tail := out[1]["content"].([]map[string]any)
				want := 1
				if multi && role != message.Role("tool") {
					want = 2
				}
				if len(tail) != want {
					t.Fatalf("attribution=%v", tail)
				}
			}
		})
	}
}

func TestAnthropicFormatterFilteredBlocksKeepResultGroup(t *testing.T) {
	msg := message.AssistantMsg("other", []message.ContentBlock{
		message.TextBlock{Type: "text", Text: ""},
		message.ToolResultBlock{Type: "tool_result", ID: "a", Output: "one"},
		message.TextBlock{Type: "text", Text: ""},
		message.DataBlock{Type: "data", Source: message.Base64Source{Type: "base64", MediaType: "audio/ogg", Data: "abc"}},
		message.ToolResultBlock{Type: "tool_result", ID: "b", Output: "two"},
	})
	f := NewAnthropicFormatter()
	out, err := f.FormatMultiAgent([]*message.Msg{msg}, "self")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0]["role"] != "user" {
		t.Fatalf("messages=%v", out)
	}
	blocks := out[0]["content"].([]map[string]any)
	if len(blocks) != 2 || blocks[0]["tool_use_id"] != "a" || blocks[1]["tool_use_id"] != "b" {
		t.Fatalf("results=%v", blocks)
	}
	empty := message.AssistantMsg("other", []message.ContentBlock{message.TextBlock{Type: "text", Text: ""}})
	out, err = f.FormatMultiAgent([]*message.Msg{empty}, "self")
	if err != nil || len(out) != 0 {
		t.Fatalf("empty emitted: %v %v", out, err)
	}
}
