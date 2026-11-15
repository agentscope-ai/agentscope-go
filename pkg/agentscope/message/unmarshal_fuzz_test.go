package message

import (
	"encoding/json"
	"testing"
)

// FuzzUnmarshalContentBlocks ensures the content-block decoder never panics on
// arbitrary/malformed JSON (it decodes untrusted persisted/model data).
func FuzzUnmarshalContentBlocks(f *testing.F) {
	seeds := []string{
		`[{"type":"text","text":"hi"}]`,
		`[{"type":"tool_result","output":[{"type":"text","text":"x"}]}]`,
		`[{"type":"data","source":{"type":"base64","data":"aGk=","media_type":"image/png"}}]`,
		`[{"type":"unknown"}]`,
		`[]`,
		`null`,
		`{`,
		`[{"type":"tool_call","input":"{bad json"}]`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data string) {
		blocks, err := UnmarshalContentBlocks(json.RawMessage(data))
		if err != nil {
			return
		}
		// Round-trip anything that decoded, to exercise the marshal path too.
		if _, err := json.Marshal(blocks); err != nil {
			t.Errorf("marshal of decoded blocks failed for %q: %v", data, err)
		}
	})
}
