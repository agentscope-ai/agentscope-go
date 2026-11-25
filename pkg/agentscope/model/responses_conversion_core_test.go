package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/formatter"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResponsesConversionCoreWire(t *testing.T) {
	image := message.DataBlock{Type: "data", Source: message.URLSource{MediaType: "image/png", URL: "https://example.com/image.png"}}
	raw := map[string]any{"type": "reasoning", "id": "rs_1", "encrypted_content": "opaque", "omit": nil}
	msg := &message.Msg{Role: message.RoleAssistant, Name: "bot", Content: []message.ContentBlock{
		message.ThinkingBlock{Type: "thinking", Extra: map[string]any{"responses_item": raw}},
		message.TextBlock{Type: "text", Text: "before"},
		message.ToolCallBlock{Type: "tool_call", ID: "fc_1", Name: "lookup", Input: "{}", Extra: map[string]any{"call_id": "call_1"}},
		message.TextBlock{Type: "text", Text: "after"},
		message.ToolResultBlock{Type: "tool_result", ID: "fc_1", Output: []message.ContentBlock{message.TextBlock{Type: "text", Text: "found"}, image}},
		message.HintBlock{Type: "hint", Hint: []message.ContentBlock{message.TextBlock{Type: "text", Text: "look"}, image}},
	}}
	expected := []map[string]any{
		{"type": "reasoning", "id": "rs_1", "encrypted_content": "opaque"},
		{"role": "assistant", "content": "before"},
		{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "lookup", "arguments": "{}"},
		{"role": "assistant", "content": "after"},
		{"type": "function_call_output", "call_id": "call_1", "output": []map[string]any{{"type": "input_text", "text": "found"}, {"type": "input_image", "image_url": "https://example.com/image.png"}}},
		{"role": "user", "content": []map[string]any{{"type": "input_text", "text": "look"}, {"type": "input_image", "image_url": "https://example.com/image.png"}}},
	}
	msgs := []*message.Msg{nil, message.SystemMsg("s", "system"), msg}
	before, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	f := formatter.NewOpenAIResponseFormatter()
	for _, multi := range []bool{false, true} {
		var got []map[string]any
		if multi {
			got, err = f.FormatMultiAgent(msgs, "other")
		} else {
			got, err = f.Format(msgs)
		}
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("formatter multi=%t: got=%v want=%v", multi, got, expected)
		}
	}
	wantJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	var want any
	if err = json.Unmarshal(wantJSON, &want); err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			m := newResponsesStreamModel(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
					t.Error(e)
					return
				}
				if !reflect.DeepEqual(body["input"], want) {
					t.Errorf("wire input=%v want=%v", body["input"], want)
				}
				if body["instructions"] != "system" || body["stream"] != stream {
					t.Errorf("envelope=%v", body)
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, sseCompletedLine())
				} else {
					fmt.Fprint(w, `{"id":"resp_1","status":"completed","output":[]}`)
				}
			})
			if !stream {
				if _, e := m.Chat(context.Background(), msgs); e != nil {
					t.Fatal(e)
				}
				return
			}
			ch, e := m.ChatStream(context.Background(), msgs)
			if e != nil {
				t.Fatal(e)
			}
			for r := range ch {
				if r.Error != nil {
					t.Fatal(r.Error)
				}
			}
		})
	}
	after, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("conversion mutated history")
	}
}

// A conversion extraction must retain local serialization errors, transport
// errors and provider errors without turning them into successful responses.
func TestResponsesConversionFailureBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		invalid  bool
		canceled bool
		baseURL  string
		status   int
		body     string
		want     string
	}{
		{name: "unserializable_reasoning", invalid: true, want: "marshal request"},
		{name: "canceled", canceled: true, want: "context canceled"},
		{name: "invalid_endpoint", baseURL: "http://bad\n", want: "invalid control character"},
		{name: "provider_error", status: 400, body: "rejected", want: "HTTP 400: rejected"},
		{name: "invalid_json", status: 200, body: "not json", want: "decode failed"},
	} {
		for _, stream := range []bool{false, true} {
			if tc.name == "invalid_json" && stream {
				continue
			}
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				var requests atomic.Int32
				m := newResponsesStreamModel(t, func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				})
				if tc.baseURL != "" {
					m.(*openaiResponseModel).cfg.BaseURL = tc.baseURL
				}
				msgs := []*message.Msg{message.UserMsg("u", "hello")}
				if tc.invalid {
					msgs = []*message.Msg{{Role: message.RoleAssistant, Content: []message.ContentBlock{message.ThinkingBlock{Type: "thinking", Extra: map[string]any{"responses_item": map[string]any{"type": "reasoning", "invalid": func() {}}}}}}}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.canceled {
					cancel()
				}
				var err error
				if stream {
					ch, e := m.ChatStream(ctx, msgs)
					err = e
					if ch != nil {
						t.Fatal("stream returned on request failure")
					}
				} else {
					r, e := m.Chat(ctx, msgs)
					err = e
					if r != nil {
						t.Fatal("response returned on failure")
					}
				}
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error=%v, want %q", err, tc.want)
				}
				wantRequests := int32(0)
				if tc.status != 0 {
					wantRequests = 1
				}
				if requests.Load() != wantRequests {
					t.Errorf("requests=%d want=%d", requests.Load(), wantRequests)
				}
			})
		}
	}
}
