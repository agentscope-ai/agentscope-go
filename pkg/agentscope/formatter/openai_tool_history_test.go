package formatter

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestOpenAIFamilyMixedToolHistory(t *testing.T) {
	blocks := []message.ContentBlock{
		message.TextBlock{Type: "text", Text: "Checking two cities."},
		message.ThinkingBlock{Type: "thinking", Thinking: "Compare the forecasts."},
		message.ToolCallBlock{Type: "tool_call", ID: "a", Name: "weather", Input: `{"city":"Shanghai"}`},
		message.ToolCallBlock{Type: "tool_call", ID: "b", Name: "weather", Input: `{"city":"Hangzhou"}`},
		message.ToolResultBlock{Type: "tool_result", ID: "a", Name: "weather", Output: "18 C"},
		message.ToolResultBlock{Type: "tool_result", ID: "b", Name: "weather", Output: "20 C"},
		message.TextBlock{Type: "text", Text: "One more city."},
		message.ToolCallBlock{Type: "tool_call", ID: "c", Name: "weather", Input: `{"city":"Suzhou"}`},
		message.ToolResultBlock{Type: "tool_result", ID: "c", Name: "weather", Output: "19 C"},
		message.TextBlock{Type: "text", Text: "All forecasts ready."},
	}
	call := func(id, city string) map[string]any {
		return map[string]any{"id": id, "type": "function", "function": map[string]any{
			"name": "weather", "arguments": `{"city":"` + city + `"}`,
		}}
	}
	for _, nf := range allFormatters() {
		switch nf.name {
		case "Anthropic", "Gemini", "OpenAIResponse":
			continue
		}
		t.Run(nf.name, func(t *testing.T) {
			want := []map[string]any{
				{"role": "assistant", "content": "Checking two cities.", "tool_calls": []map[string]any{call("a", "Shanghai"), call("b", "Hangzhou")}},
				{"role": "tool", "tool_call_id": "a", "content": "18 C"},
				{"role": "tool", "tool_call_id": "b", "content": "20 C"},
				{"role": "assistant", "content": "One more city.", "tool_calls": []map[string]any{call("c", "Suzhou")}},
				{"role": "tool", "tool_call_id": "c", "content": "19 C"},
				{"role": "assistant", "content": "All forecasts ready."},
			}
			if nf.name == "DeepSeek" || nf.name == "DashScope" {
				want[0]["reasoning_content"] = "Compare the forecasts."
			}
			if nf.name == "Ollama" {
				for _, i := range []int{1, 2, 4} {
					want[i]["tool_name"] = "weather"
				}
			}
			msg := message.AssistantMsg("weather-agent", blocks)
			before, err := json.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			got, err := nf.f.Format([]*message.Msg{nil, msg})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("mixed tool history:\n got %#v\nwant %#v", got, want)
			}
			after, err := json.Marshal(msg)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("formatter changed stored history: %s; error: %v", after, err)
			}
		})
	}
}

func TestOpenAIFormatterMixedHistoryInvalidMedia(t *testing.T) {
	msg := message.AssistantMsg("agent", []message.ContentBlock{
		message.ToolCallBlock{Type: "tool_call", ID: "a", Name: "weather", Input: `{}`},
		message.ToolResultBlock{Type: "tool_result", ID: "a", Output: "18 C"},
		message.DataBlock{Type: "data", Source: message.Base64Source{Type: "base64", MediaType: "audio/ogg", Data: "AAAA"}},
	})
	for _, multiAgent := range []bool{false, true} {
		f := NewOpenAIFormatter()
		var got []map[string]any
		var err error
		if multiAgent {
			got, err = f.FormatMultiAgent([]*message.Msg{msg}, "agent")
		} else {
			got, err = f.Format([]*message.Msg{msg})
		}
		if err == nil || got != nil {
			t.Fatalf("invalid media should return an error without partial history: got %#v, error %v", got, err)
		}
	}
}

func TestOpenAIFormatterToolResultsWithoutEmptySegments(t *testing.T) {
	got, err := NewOpenAIFormatter().Format([]*message.Msg{message.AssistantMsg("agent", []message.ContentBlock{
		message.ToolResultBlock{Type: "tool_result", ID: "a", Output: ""},
		message.ToolResultBlock{Type: "tool_result", ID: "b", Output: "failed", State: message.ToolResultError},
	})})
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"role": "tool", "tool_call_id": "a", "content": ""},
		{"role": "tool", "tool_call_id": "b", "content": "failed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestOpenAIMultiAgentExpandedToolHistory(t *testing.T) {
	msgs := []*message.Msg{
		nil,
		message.AssistantMsg("planner", "Check the weather."),
		message.AssistantMsg("weather-agent", []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "a", Name: "weather", Input: `{}`},
			message.ToolResultBlock{Type: "tool_result", ID: "a", Output: "18 C"},
			message.TextBlock{Type: "text", Text: "It is mild."},
		}),
		message.UserMsg("reader", "Thanks."),
	}
	got, err := NewOpenAIFormatter().FormatMultiAgent(msgs, "current")
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"role": "assistant", "name": "planner", "content": "Check the weather."},
		{"role": "assistant", "name": "weather_agent", "content": nil, "tool_calls": []map[string]any{{
			"id": "a", "type": "function", "function": map[string]any{"name": "weather", "arguments": `{}`},
		}}},
		{"role": "tool", "tool_call_id": "a", "content": "18 C"},
		{"role": "assistant", "name": "weather_agent", "content": "It is mild."},
		{"role": "user", "name": "reader", "content": "Thanks."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded multi-agent history:\n got %#v\nwant %#v", got, want)
	}
}

func TestOpenAIMultiAgentPreservesStructuredMessages(t *testing.T) {
	for _, blocks := range [][]message.ContentBlock{
		{message.ThinkingBlock{Type: "thinking", Thinking: "reason"}, message.TextBlock{Type: "text", Text: "answer"}},
		{message.DataBlock{Type: "data", Source: message.URLSource{Type: "url", URL: "https://example.com/image.png", MediaType: "image/png"}}},
	} {
		f := NewDashScopeFormatter()
		msgs := []*message.Msg{message.AssistantMsg("a", "first"), message.AssistantMsg("b", blocks)}
		got, err := f.FormatMultiAgent(msgs, "current")
		if err != nil {
			t.Fatal(err)
		}
		want, err := f.Format(msgs)
		if err != nil {
			t.Fatal(err)
		}
		want[0]["name"], want[1]["name"] = "a", "b"
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("structured messages changed during merge: got %#v, want %#v", got, want)
		}
	}
}

func TestOpenAIMultiAgentMergesPlainText(t *testing.T) {
	for _, tt := range []struct {
		name, sender, current, want string
	}{
		{"other sender", "b", "current", "first\n[b]: second"},
		{"current sender", "b", "b", "first\nsecond"},
		{"unnamed sender", "", "current", "first\nsecond"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewOpenAIFormatter().FormatMultiAgent([]*message.Msg{
				message.AssistantMsg("a", "first"), message.AssistantMsg(tt.sender, "second"),
			}, tt.current)
			if err != nil {
				t.Fatal(err)
			}
			want := []map[string]any{{"role": "assistant", "name": "a", "content": tt.want}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("plain text merge: got %#v, want %#v", got, want)
			}
		})
	}
}
