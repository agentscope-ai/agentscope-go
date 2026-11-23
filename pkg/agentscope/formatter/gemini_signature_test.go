package formatter

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestGeminiFormatterToolCallSignatures(t *testing.T) {
	for _, multi := range []bool{false, true} {
		name := "Format"
		if multi {
			name = "FormatMultiAgent"
		}
		t.Run(name, func(t *testing.T) {
			blocks := []message.ContentBlock{
				message.ToolCallBlock{Type: "tool_call", ID: "local_1", Name: "weather", Input: `{"city":"Paris"}`, Extra: map[string]any{"thought_signature": "opaque+/=signature-1"}},
				message.ToolCallBlock{Type: "tool_call", ID: "local_2", Name: "time", Input: `{}`, Extra: map[string]any{"thought_signature": "signature-2"}},
				message.ToolCallBlock{Type: "tool_call", ID: "old", Name: "unsigned", Input: `{}`},
				message.ToolCallBlock{Type: "tool_call", ID: "", Name: "empty", Input: `{}`, Extra: map[string]any{"thought_signature": ""}},
				message.ToolCallBlock{Type: "tool_call", ID: "invalid", Name: "invalid", Input: `{}`, Extra: map[string]any{"thought_signature": 42}},
			}
			msgs := []*message.Msg{{Role: message.RoleAssistant, Name: "peer", Content: blocks}}
			saved, err := json.Marshal(msgs)
			if err != nil {
				t.Fatal(err)
			}
			var restored []*message.Msg
			if err = json.Unmarshal(saved, &restored); err != nil {
				t.Fatal(err)
			}
			f := NewGeminiFormatter()
			var got []map[string]any
			if multi {
				got, err = f.FormatMultiAgent(restored, "current")
			} else {
				got, err = f.Format(restored)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0]["role"] != "model" {
				t.Fatalf("messages=%v", got)
			}
			parts, ok := got[0]["parts"].([]map[string]any)
			if !ok {
				t.Fatalf("parts=%v", got[0]["parts"])
			}
			if multi {
				if len(parts) == 0 || parts[0]["text"] != "[peer]: " {
					t.Fatalf("sender prefix=%v", parts)
				}
				parts = parts[1:]
			}
			if len(parts) != len(blocks) {
				t.Fatalf("parts=%v", parts)
			}
			signatures := []string{"opaque+/=signature-1", "signature-2", "", "", ""}
			for i, part := range parts {
				call := blocks[i].(message.ToolCallBlock)
				args := map[string]any{}
				if i == 0 {
					args["city"] = "Paris"
				}
				want := map[string]any{"functionCall": map[string]any{"name": call.Name, "args": args}}
				if call.ID != "" {
					want["functionCall"].(map[string]any)["id"] = call.ID
				}
				if signatures[i] != "" {
					want["thoughtSignature"] = signatures[i]
				}
				if !reflect.DeepEqual(part, want) {
					t.Errorf("part[%d]=%#v, want %#v", i, part, want)
				}
			}
			after, err := json.Marshal(restored)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(saved, after) {
				t.Error("formatter mutated stored history")
			}
		})
	}
}
