package model

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestGeminiStreamFinalToolAndTermination(t *testing.T) {
	for _, reason := range []string{"", "STOP", "MAX_TOKENS"} {
		t.Run("finish="+reason, func(t *testing.T) {
			in := make(chan httpx.SSEEvent, 2)
			in <- httpx.SSEEvent{Data: `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"weather","args":{"city":"Shanghai"}}}]}}]}`}
			if reason != "" {
				in <- httpx.SSEEvent{Data: `{"candidates":[{"finishReason":"` + reason + `"}]}`}
			}
			close(in)
			out := make(chan ChatResponse, 8)
			processGeminiStream(context.Background(), in, out)
			var last ChatResponse
			for chunk := range out {
				last = chunk
			}
			if !last.IsLast {
				t.Fatal("no final")
			}
			if reason == "" && last.Error == nil {
				t.Error("missing completion accepted")
			}
			if reason == "STOP" && last.Error != nil {
				t.Fatal(last.Error)
			}
			if reason == "MAX_TOKENS" && last.StopReason != StopReasonLength {
				t.Errorf("stop reason=%q", last.StopReason)
			}
			if len(last.Content) != 1 {
				t.Fatalf("tool lost from final: %+v", last)
			}
			if tc, ok := last.Content[0].(message.ToolCallBlock); !ok || tc.Name != "weather" {
				t.Fatalf("tool=%+v", last.Content)
			}
		})
	}
}

func TestAnthropicStreamStopReason(t *testing.T) {
	in := make(chan httpx.SSEEvent, 2)
	in <- httpx.SSEEvent{Event: "message_delta", Data: `{"delta":{"stop_reason":"max_tokens"}}`}
	in <- httpx.SSEEvent{Event: "message_stop", Data: `{"type":"message_stop"}`}
	close(in)
	out := make(chan ChatResponse, 8)
	processAnthropicStream(context.Background(), in, out)
	for last := range out {
		if last.IsLast && last.StopReason != StopReasonLength {
			t.Fatalf("stop reason=%q", last.StopReason)
		}
	}
}

func TestAnthropicStreamInvalidEvents(t *testing.T) {
	for _, tc := range []struct{ name, event, data string }{
		{"malformed", "content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":3}}`},
		{"negative", "content_block_start", `{"index":-1,"content_block":{"type":"text"}}`},
		{"huge", "content_block_start", `{"index":2147483647,"content_block":{"type":"text"}}`},
		{"unstarted", "content_block_delta", `{"index":2,"delta":{"type":"text_delta","text":"bad"}}`},
		{"stop_reason", "message_delta", `{"delta":{"stop_reason":3}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := make(chan httpx.SSEEvent, 3)
			in <- httpx.SSEEvent{Event: "content_block_start", Data: `{"index":0,"content_block":{"type":"tool_use","id":"call","name":"write","input":{}}}`}
			in <- httpx.SSEEvent{Event: tc.event, Data: tc.data}
			in <- httpx.SSEEvent{Event: "message_stop", Data: `{"type":"message_stop"}`}
			close(in)
			out := make(chan ChatResponse, 8)
			processAnthropicStream(context.Background(), in, out)
			var last ChatResponse
			for chunk := range out {
				last = chunk
			}
			if last.Error == nil {
				t.Fatalf("invalid event accepted: %+v", last)
			}
		})
	}
}

func TestAnthropicStreamInitialContent(t *testing.T) {
	in := make(chan httpx.SSEEvent, 5)
	in <- httpx.SSEEvent{Event: "content_block_start", Data: `{"index":0,"content_block":{"type":"text","text":"initial"}}`}
	in <- httpx.SSEEvent{Event: "content_block_stop", Data: `{"index":0}`}
	in <- httpx.SSEEvent{Event: "content_block_start", Data: `{"index":1,"content_block":{"type":"tool_use","id":"call","name":"weather","input":{"city":"Shanghai"}}}`}
	in <- httpx.SSEEvent{Event: "content_block_stop", Data: `{"index":1}`}
	in <- httpx.SSEEvent{Event: "message_stop", Data: `{"type":"message_stop"}`}
	close(in)
	out := make(chan ChatResponse, 8)
	processAnthropicStream(context.Background(), in, out)
	var first, last ChatResponse
	for chunk := range out {
		if !chunk.IsLast {
			first = chunk
		}
		last = chunk
	}
	if first.GetTextContent() != "initial" || last.Error != nil || last.GetTextContent() != "initial" || len(last.Content) != 2 {
		t.Fatalf("first=%+v last=%+v", first, last)
	}
	tc, ok := last.Content[1].(message.ToolCallBlock)
	if !ok || tc.Input != `{"city":"Shanghai"}` {
		t.Fatal(last.Content)
	}
}
