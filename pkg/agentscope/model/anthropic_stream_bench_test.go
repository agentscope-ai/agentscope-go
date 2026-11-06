package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Fixtures are encoded before timing. Each operation runs the real SSE event
// parser, including JSON decoding, channel allocation/queueing, delta emission
// and final assembly. HTTP transport and provider latency are not measured.
func BenchmarkAnthropicStreamAccumulation(b *testing.B) {
	for _, kind := range []string{"text", "thinking", "tool_use", "signature"} {
		for _, size := range []struct{ bytes, chunk int }{
			{1024, 32}, {1024, 1024}, {32768, 32}, {131072, 32}, {131072, 512},
		} {
			b.Run(fmt.Sprintf("%s/bytes=%d/chunk=%d", kind, size.bytes, size.chunk), func(b *testing.B) {
				events, want := anthropicAccumulationFixture(kind, size.bytes, size.chunk)
				b.SetBytes(int64(len(want)))
				b.ReportAllocs()
				b.ResetTimer()
				var last ChatResponse
				for n := 0; n < b.N; n++ {
					last, _ = parseAnthropicAccumulation(events, anthropicStreamPolicy{}, false)
					if !last.IsLast || last.Error != nil {
						b.Fatalf("incomplete stream: %+v", last)
					}
				}
				b.StopTimer()
				if got := anthropicAccumulatedValue(&last, kind); got != want {
					b.Fatalf("assembled %s differs: got %d bytes, want %d", kind, len(got), len(want))
				}
			})
		}
	}
}

func anthropicAccumulationFixture(kind string, size, chunk int) ([]httpx.SSEEvent, string) {
	payload := strings.Repeat("x", size)
	deltaType, field := kind+"_delta", kind
	initial := map[string]any{"type": kind}
	switch kind {
	case "tool_use":
		payload = `{"value":"` + strings.Repeat("x", size-12) + `"}`
		deltaType, field = "input_json_delta", "partial_json"
		initial["id"], initial["name"], initial["input"] = "call_1", "write", map[string]any{}
	case "signature":
		initial["type"], initial["thinking"] = "thinking", ""
	default:
		initial[kind] = ""
	}
	events := []httpx.SSEEvent{
		{Event: "message_start", Data: `{"message":{"id":"msg_bench","model":"fixture","usage":{"input_tokens":11}}}`},
		anthropicAccumulationEvent("content_block_start", map[string]any{"index": 0, "content_block": initial}),
	}
	for offset := 0; offset < len(payload); offset += chunk {
		end := min(offset+chunk, len(payload))
		events = append(events, anthropicAccumulationEvent("content_block_delta", map[string]any{
			"index": 0, "delta": map[string]any{"type": deltaType, field: payload[offset:end]},
		}))
	}
	events = append(events,
		httpx.SSEEvent{Event: "content_block_stop", Data: `{"index":0}`},
		httpx.SSEEvent{Event: "message_delta", Data: `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`},
		httpx.SSEEvent{Event: "message_stop", Data: `{"type":"message_stop"}`},
	)
	return events, payload
}

func anthropicAccumulationEvent(event string, value any) httpx.SSEEvent {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err) // Test fixtures contain only JSON-compatible values.
	}
	return httpx.SSEEvent{Event: event, Data: string(data)}
}

func parseAnthropicAccumulation(events []httpx.SSEEvent, policy anthropicStreamPolicy, capture bool) (ChatResponse, []message.ContentBlock) {
	in := make(chan httpx.SSEEvent, len(events))
	for _, evt := range events {
		in <- evt
	}
	close(in)
	out := make(chan ChatResponse, len(events)+1)
	processAnthropicStreamWithPolicy(context.Background(), in, out, policy)
	var last ChatResponse
	var deltas []message.ContentBlock
	for resp := range out {
		if resp.IsLast {
			last = resp
		} else if capture {
			deltas = append(deltas, resp.Content...)
		}
	}
	return last, deltas
}

func anthropicAccumulatedValue(resp *ChatResponse, kind string) string {
	if len(resp.Content) != 1 {
		return ""
	}
	switch block := resp.Content[0].(type) {
	case message.TextBlock:
		return block.Text
	case message.ThinkingBlock:
		if kind == "signature" {
			signature, _ := block.Extra["signature"].(string)
			return signature
		}
		return block.Thinking
	case message.ToolCallBlock:
		return block.Input
	}
	return ""
}
