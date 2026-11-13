package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestResponsesToolChoiceWire(t *testing.T) {
	for _, tc := range []struct {
		name    string
		choice  *ToolChoice
		want    any
		present bool
	}{
		{"unset", nil, nil, false},
		{"auto", &ToolChoice{Mode: "auto"}, "auto", true},
		{"none", &ToolChoice{Mode: "none"}, "none", true},
		{"required", &ToolChoice{Mode: "required"}, "required", true},
		{"named", &ToolChoice{Mode: "get_weather"}, map[string]any{"type": "function", "name": "get_weather"}, true},
		{"another_name", &ToolChoice{Mode: "get_time"}, map[string]any{"type": "function", "name": "get_time"}, true},
		{"empty", &ToolChoice{}, "", true},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				m := newResponsesStreamModel(t, func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						http.Error(w, "decode", 400)
						return
					}
					if r.URL.Path != "/v1/responses" || body["stream"] != stream {
						t.Errorf("unexpected request: %s stream=%v", r.URL.Path, body["stream"])
					}
					got, present := body["tool_choice"]
					if present != tc.present || !reflect.DeepEqual(got, tc.want) {
						t.Errorf("tool_choice=%#v (present=%t), want %#v (present=%t)", got, present, tc.want, tc.present)
					}
					wantTools := []any{
						map[string]any{"type": "function", "name": "get_weather", "description": "weather", "parameters": map[string]any{"type": "object"}},
						map[string]any{"type": "function", "name": "get_time", "description": "time", "parameters": map[string]any{"type": "object"}},
					}
					if !reflect.DeepEqual(body["tools"], wantTools) {
						t.Errorf("tools=%#v, want %#v", body["tools"], wantTools)
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, sseCompletedLine())
					} else {
						fmt.Fprint(w, `{"id":"resp_1","status":"completed","output":[]}`)
					}
				})
				tools := []ToolSchema{
					{Type: "function", Function: ToolFunction{Name: "get_weather", Description: "weather", Parameters: json.RawMessage(`{"type":"object"}`)}},
					{Type: "function", Function: ToolFunction{Name: "get_time", Description: "time", Parameters: json.RawMessage(`{"type":"object"}`)}},
				}
				opts := []CallOption{WithTools(tools), WithToolChoice(tc.choice)}
				msgs := []*message.Msg{message.UserMsg("u", "check")}
				if !stream {
					if _, err := m.Chat(context.Background(), msgs, opts...); err != nil {
						t.Fatal(err)
					}
					return
				}
				ch, err := m.ChatStream(context.Background(), msgs, opts...)
				if err != nil {
					t.Fatal(err)
				}
				lasts := 0
				for r := range ch {
					if r.Error != nil {
						t.Fatal(r.Error)
					}
					if r.IsLast {
						lasts++
					}
				}
				if lasts != 1 {
					t.Fatalf("terminal responses=%d", lasts)
				}
			})
		}
	}
}
