package formatter

import (
	"reflect"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestResponsesForeignItemID(t *testing.T) {
	for _, tc := range []struct {
		id     string
		native bool
	}{
		{"chatcmpl-tool-123", false}, {"call_123", false}, {"toolu_123", false}, {"", false}, {"FC_123", false}, {"fc_123", true}, {"fc123", true},
	} {
		t.Run(tc.id, func(t *testing.T) {
			for _, override := range []bool{false, true} {
				callID := tc.id
				var extra map[string]any
				if override {
					callID = "call_original"
					extra = map[string]any{"call_id": callID}
				}
				msgs := []*message.Msg{message.NewMsg("assistant", message.RoleAssistant, []message.ContentBlock{
					message.ToolCallBlock{ID: tc.id, Name: "lookup", Input: "{}", Extra: extra},
					message.ToolResultBlock{ID: tc.id, Output: "ok", Metadata: map[string]any{"call_id": callID}},
				})}
				f := NewOpenAIResponseFormatter()
				items, err := f.Format(msgs)
				if err != nil {
					t.Fatal(err)
				}
				multi, err := f.FormatMultiAgent(msgs, "assistant")
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(items, multi) {
					t.Errorf("multi-agent output differs: %v", multi)
				}
				if len(items) != 2 {
					t.Fatalf("items=%v", items)
				}
				id, present := items[0]["id"]
				if present != tc.native || (present && id != tc.id) {
					t.Errorf("item ID: %v", items[0])
				}
				if items[0]["call_id"] != callID || items[1]["call_id"] != callID {
					t.Errorf("correlation changed: %v", items)
				}
			}
		})
	}
}
