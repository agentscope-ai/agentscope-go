package formatter

import (
	"bytes"
	"encoding/json"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"reflect"
	"testing"
)

func TestResponsesCoreHistory(t *testing.T) {
	image := message.DataBlock{Type: "data", Source: message.Base64Source{MediaType: "image/png", Data: "QUJD"}}
	url := message.DataBlock{Type: "data", Source: message.URLSource{MediaType: "image/png", URL: "https://example.com/image.png"}}
	audio := message.DataBlock{Type: "data", Source: message.Base64Source{MediaType: "audio/wav", Data: "QUJD"}}
	f := NewOpenAIResponseFormatter()
	raw := map[string]any{"type": "reasoning", "id": "rs", "encrypted_content": "opaque", "omit": nil, "summary": []any{map[string]any{"type": "summary_text", "text": "summary", "omit": nil}}}
	msgs := []*message.Msg{nil, message.SystemMsg("s", "system"), {Role: message.RoleAssistant, Content: []message.ContentBlock{
		message.ThinkingBlock{Type: "thinking", Extra: map[string]any{"responses_item": raw}},
		message.ToolCallBlock{Type: "tool_call", ID: "fc", Name: "lookup", Input: "{}", Extra: map[string]any{"call_id": "call"}},
	}}, {Role: message.RoleUser, Content: []message.ContentBlock{
		message.ToolResultBlock{Type: "tool_result", ID: "fc", Output: []message.ContentBlock{message.TextBlock{Type: "text", Text: "first"}, image, audio, url, message.TextBlock{Type: "text", Text: "last"}}},
		message.HintBlock{Type: "hint", Hint: []message.ContentBlock{message.TextBlock{Type: "text", Text: "look"}, url, image, audio}},
	}}}
	before, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.Format(msgs)
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"type": "reasoning", "id": "rs", "encrypted_content": "opaque", "summary": []any{map[string]any{"type": "summary_text", "text": "summary"}}},
		{"type": "function_call", "id": "fc", "call_id": "call", "name": "lookup", "arguments": "{}"},
		{"type": "function_call_output", "call_id": "call", "output": []map[string]any{{"type": "input_text", "text": "first"}, {"type": "input_image", "image_url": "data:image/png;base64,QUJD"}, {"type": "input_text", "text": "[audio/wav data]"}, {"type": "input_image", "image_url": "https://example.com/image.png"}, {"type": "input_text", "text": "last"}}},
		{"role": "user", "content": []map[string]any{{"type": "input_text", "text": "look"}, {"type": "input_image", "image_url": "https://example.com/image.png"}, {"type": "input_image", "image_url": "data:image/png;base64,QUJD"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got=%v want=%v", got, want)
	}
	multi, err := f.FormatMultiAgent(msgs, "other")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(multi, want) {
		t.Errorf("multi-agent=%v want=%v", multi, want)
	}

	after, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("mutated history")
	}
	again, err := f.Format(msgs[3:])
	if err != nil {
		t.Fatal(err)
	}
	if again[0]["call_id"] != "fc" {
		t.Errorf("call mapping leaked: %v", again)
	}
}

func TestResponsesCoreFallbackAndMediaFilter(t *testing.T) {
	image := message.DataBlock{Type: "data", Source: message.URLSource{MediaType: "image/png", URL: "https://example.com/image.png"}}
	for _, tc := range []struct {
		name   string
		blocks []message.ContentBlock
		want   any
	}{
		{"empty", nil, ""},
		{"plain_result", []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: "fc", Output: "ok"}}, "ok"},
		{"text_result", []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: "fc", Output: []message.ContentBlock{message.TextBlock{Type: "text", Text: "a"}, message.TextBlock{Type: "text", Text: "b"}}}}, "a\nb"},
		{"string_hint", []message.ContentBlock{message.HintBlock{Type: "hint", Hint: "look"}}, "look"},
		{"unsupported_hint", []message.ContentBlock{message.HintBlock{Type: "hint", Hint: []message.ContentBlock{message.ThinkingBlock{Type: "thinking"}}}}, "[content omitted: this turn carried only block types the Responses API cannot replay]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewOpenAIResponseFormatter().Format([]*message.Msg{{Role: message.RoleUser, Content: tc.blocks}})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("items=%v", got)
			}
			field := "content"
			if got[0]["type"] == "function_call_output" {
				field = "output"
			}
			if !reflect.DeepEqual(got[0][field], tc.want) {
				t.Errorf("got=%v want=%v", got, tc.want)
			}
		})
	}
	f := NewOpenAIResponseFormatter()
	f.SupportedInputMediaTypes = nil
	got, err := f.Format([]*message.Msg{{Role: message.RoleUser, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "keep"}, image, message.HintBlock{Type: "hint", Hint: []message.ContentBlock{image, message.TextBlock{Type: "text", Text: "hint"}}}, message.ToolResultBlock{Type: "tool_result", ID: "fc", Output: []message.ContentBlock{image}}}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"role": "user", "content": "keep"}, {"role": "user", "content": []map[string]any{{"type": "input_text", "text": "hint"}}}, {"type": "function_call_output", "call_id": "fc", "output": "[image/png: https://example.com/image.png]"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filtered=%v want=%v", got, want)
	}
}
