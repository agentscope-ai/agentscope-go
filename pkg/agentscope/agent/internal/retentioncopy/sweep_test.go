package retentioncopy

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// Exhausting budgets at successive fields exercises transactional failure, not
// merely the top-level admission check: earlier copied fields never escape.
func TestBudgetFailureAtEveryCopyStage(t *testing.T) {
	values := []any{
		message.Msg{ID: "id", Name: "host", Role: message.RoleUser, Content: []message.ContentBlock{message.TextBlock{Text: "text"}}, Metadata: types.JSONObject{"k": true}, Timestamp: "time", FinishedAt: "end", Usage: &message.Usage{InputTokens: 1, OutputTokens: 2, CacheCreationInputTokens: 3, CacheInputTokens: 4}},
		message.TextBlock{Type: "text", ID: "id", Text: "text"},
		message.ThinkingBlock{Type: "thinking", ID: "id", Thinking: "thought", Extra: map[string]any{"signature": []byte{1}}},
		message.RedactedThinkingBlock{Type: "redacted", ID: "id", Data: "opaque"},
		message.ToolCallBlock{Type: "tool_call", ID: "id", Name: "name", Input: "{}", State: message.ToolCallPending, SuggestedRules: []any{"rule"}, Extra: map[string]any{"x": true}},
		message.ToolResultBlock{Type: "tool_result", ID: "id", Name: "name", State: message.ToolResultError, Output: []message.ContentBlock{message.TextBlock{Text: "result"}}, Metadata: map[string]any{"x": true}},
		message.HintBlock{Type: "hint", ID: "id", Source: "host", Hint: []message.ContentBlock{message.TextBlock{Text: "hint"}}},
		message.DataBlock{Type: "data", ID: "id", Name: "name", Source: &message.URLSource{Type: "url", URL: "never-fetch", MediaType: "image/png"}},
		message.Base64Source{Type: "base64", Data: "data", MediaType: "image/png"},
		message.URLSource{Type: "url", URL: "never-fetch", MediaType: "image/png"},
		message.Usage{InputTokens: 1, OutputTokens: 2, CacheCreationInputTokens: 3, CacheInputTokens: 4},
		[]byte{1, 2}, json.RawMessage{1, 2}, json.Number("123"), map[string]string{"key": "value"},
		&message.Msg{ID: "id", Metadata: types.JSONObject{"deep": []any{true}}},
	}
	c := copier(t, testLimits())
	for _, input := range values {
		owned, used, err := c.Copy(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		for _, resource := range []string{"nodes", "payload", "allocation", "depth"} {
			var count int64
			switch resource {
			case "nodes":
				count = used.Nodes
			case "payload":
				count = used.PayloadBytes
			case "allocation":
				count = used.AllocBytes
			case "depth":
				count = int64(used.Depth)
			}
			for limit := int64(1); limit < count; limit++ {
				l := testLimits()
				switch resource {
				case "nodes":
					l.MaxNodes = limit
				case "payload":
					l.MaxPayloadBytes = limit
				case "allocation":
					l.MaxAllocBytes = limit
				case "depth":
					l.MaxDepth = int(limit)
				}
				out, stats, err := copier(t, l).Copy(context.Background(), input)
				if out != nil || !errors.Is(err, ErrLimit) {
					t.Fatalf("%T/%s/%d returned %T %v", input, resource, limit, out, err)
				}
				if stats.AllocBytes > l.MaxAllocBytes || stats.Nodes > l.MaxNodes || stats.PayloadBytes > l.MaxPayloadBytes || stats.Depth > l.MaxDepth {
					t.Fatal("budget exceeded")
				}
			}
		}
		if !reflect.DeepEqual(owned, input) {
			t.Fatal("failed copy modified input")
		}
	}
}
func TestFailureDoesNotPoisonCopier(t *testing.T) {
	c := copier(t, testLimits())
	_, _, _ = c.Copy(context.Background(), make([]any, 5000))
	copyOK(t, c, "next call has a new budget")
}
func TestLateCancellation(t *testing.T) {
	c := copier(t, testLimits())
	ctx := &cancelDuringCopy{Context: context.Background()}
	// Five enters followed by the final cancellation check.
	out, _, err := c.Copy(ctx, []any{1, 2, 3, 4})
	if out != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("late cancellation returned success")
	}
}
