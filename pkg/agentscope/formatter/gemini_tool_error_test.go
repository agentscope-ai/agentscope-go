package formatter

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestGeminiFormatterToolResultStates(t *testing.T) {
	states := []message.ToolResultState{message.ToolResultError, message.ToolResultSuccess,
		message.ToolResultDenied, message.ToolResultInterrupted, message.ToolResultRunning, "", "unknown"}
	for _, multi := range []bool{false, true} {
		for _, state := range states {
			for _, output := range []any{"details", "", []message.ContentBlock{message.TextBlock{Text: "first"}, message.TextBlock{Text: "second"}}} {
				blocks := []message.ContentBlock{
					message.ToolResultBlock{Type: "tool_result", ID: "ok", Name: "control", Output: "ok", State: message.ToolResultSuccess},
					message.ToolResultBlock{Type: "tool_result", ID: "tested", Name: "lookup", Output: output, State: state},
				}
				msg := message.NewMsg("worker", message.RoleUser, blocks)
				before, err := json.Marshal(msg)
				if err != nil {
					t.Fatal(err)
				}
				f := NewGeminiFormatter()
				var got []map[string]any
				if multi {
					got, err = f.FormatMultiAgent([]*message.Msg{msg}, "leader")
				} else {
					got, err = f.Format([]*message.Msg{msg})
				}
				if err != nil {
					t.Fatal(err)
				}
				text, ok := output.(string)
				if !ok {
					text = "first\nsecond"
				}
				key := "result"
				if state == message.ToolResultError {
					key = "error"
				}
				parts := []map[string]any{
					{"functionResponse": map[string]any{"name": "control", "response": map[string]any{"result": "ok"}}},
					{"functionResponse": map[string]any{"name": "lookup", "response": map[string]any{key: text}}},
				}
				if multi {
					parts = append([]map[string]any{{"text": "[worker]: "}}, parts...)
				}
				want := []map[string]any{{"role": "user", "parts": parts}}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("multi=%v state=%q output=%#v: got %#v, want %#v", multi, state, output, got, want)
				}
				after, err := json.Marshal(msg)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("stored tool results changed")
				}
			}
		}
	}
}
