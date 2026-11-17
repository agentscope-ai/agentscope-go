package retentioncopy

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

func testLimits() Limits {
	return Limits{MaxMessages: 32, MaxBlocks: 256, MaxNodes: 4096, MaxDepth: 32, MaxPayloadBytes: 1 << 20, MaxAllocBytes: 2 << 20}
}
func TestNestedCopyIndependence(t *testing.T) {
	c, err := New(testLimits())
	if err != nil {
		t.Fatal(err)
	}
	original := []*message.Msg{{ID: "same-id", Metadata: types.JSONObject{"nested": map[string]any{"bytes": []byte{1, 2}}}, Content: []message.ContentBlock{message.ThinkingBlock{Extra: map[string]any{"signature": []any{"signed"}}}, message.ToolResultBlock{Output: []message.ContentBlock{message.HintBlock{Hint: []message.ContentBlock{message.TextBlock{Text: "original"}}}}, Metadata: map[string]any{"n": int64(9007199254740993)}}}, Usage: &message.Usage{InputTokens: 3}}}
	value, _, err := c.Copy(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	copied := value.([]*message.Msg)
	copied[0].Metadata["nested"].(map[string]any)["bytes"].([]byte)[0] = 9
	if original[0].Metadata["nested"].(map[string]any)["bytes"].([]byte)[0] != 1 {
		t.Fatal("copy retained a mutable metadata alias")
	}
	original[0].Content[0].(message.ThinkingBlock).Extra["signature"].([]any)[0] = "changed"
	if copied[0].Content[0].(message.ThinkingBlock).Extra["signature"].([]any)[0] != "signed" {
		t.Fatal("source mutation reached copy")
	}
	copied[0].Usage.InputTokens = 7
	if original[0].Usage.InputTokens != 3 {
		t.Fatal("usage pointer shared")
	}
}
