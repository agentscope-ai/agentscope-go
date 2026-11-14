package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Real adapter -> persisted history -> tool results -> real adapter request.
// Results use the agent's existing item-ID correlation convention.
func TestResponsesCallIDRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		callID any
		late   bool
		want   string
	}{
		{"distinct", "call_1", false, "call_1"},
		{"absent", nil, false, "fc_1"},
		{"empty", "", false, "fc_1"},
		{"invalid", 42, false, "fc_1"},
		{"late", "call_1", true, "call_1"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				var requests atomic.Int32
				m := newResponsesStreamModel(t, func(w http.ResponseWriter, r *http.Request) {
					var req struct {
						Input []map[string]any `json:"input"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						http.Error(w, "decode", 400)
						return
					}
					step := requests.Add(1)
					if step == 2 {
						var calls, results []string
						for _, item := range req.Input {
							switch item["type"] {
							case "function_call":
								calls = append(calls, fmt.Sprint(item["call_id"]))
								if item["id"] != fmt.Sprintf("fc_%d", len(calls)) {
									t.Errorf("changed item ID: %v", item)
								}
								if item["arguments"] != "{}" {
									t.Errorf("arguments=%v", item["arguments"])
								}
							case "function_call_output":
								results = append(results, fmt.Sprint(item["call_id"]))
							}
						}
						want := []string{tc.want, "call_2"}
						if !reflect.DeepEqual(calls, want) || !reflect.DeepEqual(results, want) {
							t.Errorf("calls=%v results=%v want=%v", calls, results, want)
						}
					}
					items := []map[string]any{}
					if step == 1 {
						items = []map[string]any{
							{"type": "function_call", "id": "fc_1", "name": "lookup", "arguments": "{}"},
							{"type": "function_call", "id": "fc_2", "call_id": "call_2", "name": "lookup", "arguments": "{}"},
						}
						if tc.callID != nil {
							items[0]["call_id"] = tc.callID
						}
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						for _, item := range items {
							added := make(map[string]any)
							for k, v := range item {
								added[k] = v
							}
							added["arguments"] = ""
							if tc.late {
								delete(added, "call_id")
							}
							data, _ := json.Marshal(map[string]any{"type": "response.output_item.added", "item": added})
							fmt.Fprintf(w, "data: %s\n\n", data)
							data, _ = json.Marshal(map[string]any{"type": "response.function_call_arguments.delta", "item_id": item["id"], "delta": "{}"})
							fmt.Fprintf(w, "data: %s\n\n", data)
							data, _ = json.Marshal(map[string]any{"type": "response.output_item.done", "item": item})
							fmt.Fprintf(w, "data: %s\n\n", data)
						}
						fmt.Fprint(w, sseCompletedLine())
					} else {
						_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_1", "status": "completed", "output": items})
					}
				})
				chat := func(msgs []*message.Msg) *ChatResponse {
					t.Helper()
					if !stream {
						r, e := m.Chat(context.Background(), msgs)
						if e != nil {
							t.Fatal(e)
						}
						return r
					}
					ch, e := m.ChatStream(context.Background(), msgs)
					if e != nil {
						t.Fatal(e)
					}
					var last *ChatResponse
					for r := range ch {
						if r.Error != nil {
							t.Fatal(r.Error)
						}
						if r.IsLast {
							v := r
							last = &v
						}
					}
					if last == nil {
						t.Fatal("no terminal response")
					}
					return last
				}
				first := chat([]*message.Msg{message.UserMsg("u", "lookup")})
				if len(first.Content) != 2 {
					t.Fatalf("calls=%v", first.Content)
				}
				msgs := []*message.Msg{{Role: message.RoleAssistant, Content: first.Content}}
				for i, b := range first.Content {
					call := b.(message.ToolCallBlock)
					if call.ID != fmt.Sprintf("fc_%d", i+1) {
						t.Fatalf("internal ID=%s", call.ID)
					}
					if i == 1 && call.Extra["call_id"] != "call_2" {
						t.Errorf("missing call metadata: %v", call.Extra)
					}
					msgs = append(msgs, &message.Msg{Role: message.RoleUser, Content: []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: call.ID, Name: call.Name, Output: "ok"}}})
				}
				saved, e := json.Marshal(msgs)
				if e != nil {
					t.Fatal(e)
				}
				var restored []*message.Msg
				if e = json.Unmarshal(saved, &restored); e != nil {
					t.Fatal(e)
				}
				chat(restored)
				after, e := json.Marshal(restored)
				if e != nil {
					t.Fatal(e)
				}
				if !bytes.Equal(saved, after) {
					t.Error("request conversion mutated history")
				}
				if requests.Load() != 2 {
					t.Errorf("requests=%d", requests.Load())
				}
			})
		}
	}
}

func TestResponsesImportedCallIDs(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		extra, metadata      map[string]any
		wantCall, wantResult string
	}{
		{"explicit", map[string]any{"call_id": "original"}, map[string]any{"call_id": "result_override"}, "original", "result_override"},
		{"inferred", map[string]any{"call_id": "original"}, nil, "original", "original"},
		{"empty", map[string]any{"call_id": ""}, map[string]any{"call_id": ""}, "fc", "fc"},
		{"invalid", map[string]any{"call_id": 42}, map[string]any{"call_id": 42}, "fc", "fc"},
		{"old", nil, nil, "fc", "fc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestResponseModel()
			msgs := []*message.Msg{{Role: message.RoleAssistant, Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "fc", Name: "lookup", Input: "{}", Extra: tc.extra}}}, {Role: message.RoleUser, Content: []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: "fc", Output: "ok", Metadata: tc.metadata}}}}
			items := m.buildRequestBody(msgs, &CallOptions{}, false)["input"].([]map[string]any)
			if items[0]["call_id"] != tc.wantCall || items[1]["call_id"] != tc.wantResult {
				t.Errorf("items=%v", items)
			}
			merged := &message.Msg{Role: message.RoleAssistant, Content: append(append([]message.ContentBlock{}, msgs[0].GetContentBlocks()...), msgs[1].GetContentBlocks()...)}
			combined := m.formatInputItems(merged)
			if !reflect.DeepEqual(items, combined) {
				t.Errorf("merged/split mismatch: %v vs %v", items, combined)
			}
			resultOnly := m.buildRequestBody(msgs[1:], &CallOptions{}, false)["input"].([]map[string]any)
			wantStandalone := "fc"
			if tc.name == "explicit" {
				wantStandalone = "result_override"
			}
			if resultOnly[0]["call_id"] != wantStandalone {
				t.Errorf("request mapping leaked: %v", resultOnly)
			}

		})
	}
}
