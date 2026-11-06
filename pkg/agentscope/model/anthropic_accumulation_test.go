package model

import (
	"fmt"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestAnthropicStreamFragmentedAccumulation(t *testing.T) {
	for _, kind := range []string{"text", "thinking", "tool_use", "signature"} {
		t.Run(kind, func(t *testing.T) {
			events, want := anthropicAccumulationFixture(kind, 32768, 31)
			last, deltas := parseAnthropicAccumulation(events, anthropicStreamPolicy{}, true)
			if !last.IsLast || last.Error != nil || anthropicAccumulatedValue(&last, kind) != want {
				t.Fatalf("fragmented %s assembly failed: error=%v", kind, last.Error)
			}
			if last.Usage == nil || last.Usage.InputTokens != 11 || last.Usage.OutputTokens != 7 || last.StopReason != StopReasonStop {
				t.Fatalf("completion metadata: %+v", last)
			}
			var emitted strings.Builder
			for _, delta := range deltas {
				switch block := delta.(type) {
				case message.TextBlock:
					emitted.WriteString(block.Text)
				case message.ThinkingBlock:
					emitted.WriteString(block.Thinking)
				default:
					t.Fatalf("unexpected delta %T", delta)
				}
			}
			if kind == "text" || kind == "thinking" {
				if emitted.String() != want || len(deltas) != (len(want)+30)/31 {
					t.Fatal("incremental content differs from final content")
				}
			} else if len(deltas) != 0 {
				t.Fatal("tool arguments and signatures must not become text deltas")
			}
		})
	}
}

func TestAnthropicStreamFragmentedPartialAssembly(t *testing.T) {
	for _, kind := range []string{"thinking", "tool_use", "signature"} {
		for _, terminal := range []string{"truncated", "error", "malformed"} {
			t.Run(kind+"/"+terminal, func(t *testing.T) {
				events, want := anthropicAccumulationFixture(kind, 1024, 31)
				// Drop block_stop, message_delta and message_stop, retaining
				// every fragment but no successful completion marker.
				events = events[:len(events)-3]
				switch terminal {
				case "error":
					events = append(events, httpx.SSEEvent{Event: "error", Data: `{"error":{"type":"overloaded_error","message":"retry later"}}`})
				case "malformed":
					events = append(events, httpx.SSEEvent{Event: "content_block_delta", Data: `{"index":`})
				}
				last, _ := parseAnthropicAccumulation(events, anthropicStreamPolicy{}, false)
				if !last.IsLast || last.Error == nil || anthropicAccumulatedValue(&last, kind) != want {
					t.Fatalf("partial %s content lost: error=%v", kind, last.Error)
				}
			})
		}
	}
}

// Force accumulator slice growth after each initialized block. A buffer must
// retain its contents and independent ownership across both growth and assembly.
func TestAnthropicStreamAccumulationAcrossBlocks(t *testing.T) {
	for _, policy := range []anthropicStreamPolicy{{}, {stopAtMessageStop: true, preserveReportedUsage: true}} {
		t.Run(fmt.Sprintf("stopAtMessageStop=%t", policy.stopAtMessageStop), func(t *testing.T) {
			var events []httpx.SSEEvent
			for i := 0; i < 18; i++ {
				kind := []string{"text", "thinking", "tool_use"}[i%3]
				initial := map[string]any{"type": kind, "text": "head", "thinking": "thought", "signature": "sig", "id": fmt.Sprintf("call_%d", i), "name": "write", "input": map[string]any{"old": true}}
				events = append(events, anthropicAccumulationEvent("content_block_start", map[string]any{"index": i, "content_block": initial}))
				var fragments []map[string]any
				switch kind {
				case "text":
					fragments = []map[string]any{{"type": "text_delta", "text": "-尾"}, {"type": "text_delta", "text": "!"}}
				case "thinking":
					fragments = []map[string]any{{"type": "thinking_delta", "thinking": "-tail"}, {"type": "signature_delta", "signature": "-one"}, {"type": "signature_delta", "signature": "-two"}}
				case "tool_use":
					// Even an empty first JSON delta resets the initial input.
					fragments = []map[string]any{{"type": "input_json_delta", "partial_json": ""}, {"type": "input_json_delta", "partial_json": `{"new":`}, {"type": "input_json_delta", "partial_json": `true}`}}
					if i == 17 {
						fragments = nil // Initial input survives without any delta.
					}
				}
				for _, fragment := range fragments {
					events = append(events, anthropicAccumulationEvent("content_block_delta", map[string]any{"index": i, "delta": fragment}))
				}
				events = append(events, anthropicAccumulationEvent("content_block_stop", map[string]any{"index": i}))
			}
			events = append(events, httpx.SSEEvent{Event: "message_stop", Data: `{"type":"message_stop"}`})
			last, deltas := parseAnthropicAccumulation(events, policy, true)
			if last.Error != nil || !last.IsLast || len(last.Content) != 18 {
				t.Fatalf("final response: %+v", last)
			}
			if len(deltas) != 30 {
				t.Fatalf("got %d deltas, want 30 initial/fragment text and thinking blocks", len(deltas))
			}
			for i, content := range last.Content {
				switch block := content.(type) {
				case message.TextBlock:
					if i%3 != 0 || block.Text != "head-尾!" {
						t.Fatalf("text block %d: %+v", i, block)
					}
				case message.ThinkingBlock:
					if i%3 != 1 || block.Thinking != "thought-tail" || block.Extra["signature"] != "sig-one-two" {
						t.Fatalf("thinking block %d: %+v", i, block)
					}
				case message.ToolCallBlock:
					want := `{"new":true}`
					if i == 17 {
						want = `{"old":true}`
					}
					if i%3 != 2 || block.ID != fmt.Sprintf("call_%d", i) || block.Name != "write" || block.Input != want || block.State != message.ToolCallPending {
						t.Fatalf("tool block %d: %+v", i, block)
					}
				default:
					t.Fatalf("unexpected final block %T", content)
				}
			}
		})
	}
}
