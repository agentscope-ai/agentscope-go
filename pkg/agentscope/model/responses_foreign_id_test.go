package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestResponsesForeignItemID(t *testing.T) {
	for _, id := range []string{"chatcmpl-tool-123", "call_123", "toolu_123", "", "FC_123", "fc_123", "fc123"} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit=%t", id, explicit), func(t *testing.T) {
				callID := id
				var extra map[string]any
				if explicit {
					callID = "call_original"
					extra = map[string]any{"call_id": callID}
				}
				msgs := []*message.Msg{message.NewMsg("assistant", message.RoleAssistant, []message.ContentBlock{
					message.ToolCallBlock{ID: id, Name: "lookup", Input: `{"b":2, "a":1}`, Extra: extra},
					message.ToolResultBlock{ID: id, Name: "lookup", Output: "ok", Metadata: map[string]any{"call_id": callID}},
				})}
				before, err := json.Marshal(msgs)
				if err != nil {
					t.Fatal(err)
				}
				check := func(items []map[string]any) {
					t.Helper()
					if len(items) != 2 {
						t.Fatalf("items=%v", items)
					}
					call := items[0]
					gotID, present := call["id"]
					if strings.HasPrefix(id, "fc") {
						if !present || gotID != id {
							t.Errorf("native item ID changed: %v", call)
						}
					} else if present {
						t.Errorf("foreign item ID must be omitted: %v", call)
					}
					if call["type"] != "function_call" || call["call_id"] != callID || call["name"] != "lookup" || call["arguments"] != `{"b":2, "a":1}` {
						t.Errorf("call changed: %v", call)
					}
					if items[1]["type"] != "function_call_output" || items[1]["call_id"] != callID || items[1]["output"] != "ok" {
						t.Errorf("result changed: %v", items[1])
					}
				}
				for _, stream := range []bool{false, true} {
					requests := 0
					m := newResponsesStreamModel(t, func(w http.ResponseWriter, r *http.Request) {
						requests++
						var body struct {
							Input []map[string]any `json:"input"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
							return
						}
						check(body.Input)
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, sseCompletedLine())
						} else {
							fmt.Fprint(w, `{"id":"resp_1","status":"completed","output":[]}`)
						}
					})
					if stream {
						ch, err := m.ChatStream(context.Background(), msgs)
						if err != nil {
							t.Fatal(err)
						}
						for resp := range ch {
							if resp.Error != nil {
								t.Fatal(resp.Error)
							}
						}
					} else if _, err := m.Chat(context.Background(), msgs); err != nil {
						t.Fatal(err)
					}
					if requests != 1 {
						t.Errorf("requests=%d", requests)
					}
				}
				after, err := json.Marshal(msgs)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("history mutated")
				}
			})
		}
	}
}
