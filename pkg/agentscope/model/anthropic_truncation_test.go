package model

import (
	"context"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
)

func anthropicTruncationEvents(withMessageStop bool) []httpx.SSEEvent {
	evts := []httpx.SSEEvent{
		{Event: "message_start", Data: `{"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":11}}}`},
		{Event: "content_block_start", Data: `{"type":"content_block_start","index":0,"content_block":{"type":"text","id":"tb1","text":""}}`},
		{Event: "content_block_delta", Data: `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`},
		{Event: "content_block_stop", Data: `{"type":"content_block_stop","index":0}`},
		{Event: "message_delta", Data: `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`},
	}
	if withMessageStop {
		evts = append(evts, httpx.SSEEvent{Event: "message_stop", Data: `{"type":"message_stop"}`})
	}
	return evts
}

func runAnthropicStream(t *testing.T, evts []httpx.SSEEvent) ChatResponse {
	t.Helper()
	sseCh := make(chan httpx.SSEEvent, len(evts))
	for _, e := range evts {
		sseCh <- e
	}
	close(sseCh)
	outCh := make(chan ChatResponse, 16)
	go processAnthropicStream(context.Background(), sseCh, outCh)
	var last ChatResponse
	sawLast := false
	for resp := range outCh {
		if resp.IsLast {
			last = resp
			sawLast = true
		}
	}
	if !sawLast {
		t.Fatal("no final IsLast response")
	}
	return last
}

// Upstream #2350: a stream that ends without message_stop is truncated and
// must surface an Error on the final response instead of ending silently.
func TestAnthropicStreamTruncatedWithoutMessageStop(t *testing.T) {
	last := runAnthropicStream(t, anthropicTruncationEvents(false))
	if last.Error == nil {
		t.Fatal("truncated stream must carry an Error")
	}
	if len(last.Content) == 0 {
		t.Error("accumulated content should still be delivered")
	}
}

func TestAnthropicStreamCompleteWithMessageStop(t *testing.T) {
	last := runAnthropicStream(t, anthropicTruncationEvents(true))
	if last.Error != nil {
		t.Errorf("complete stream must not carry an Error, got %v", last.Error)
	}
	if last.Usage == nil || last.Usage.InputTokens != 11 || last.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v, want in=11 out=7", last.Usage)
	}
}

// Anthropic reports mid-stream failures (overloaded_error, rate limits, an
// aborted generation) as an EVENT inside an otherwise successful HTTP 200
// stream. Skipping it left the only visible symptom as the misleading
// "stream ended without message_stop (truncated)".
func TestAnthropicStreamSurfacesErrorEvent(t *testing.T) {
	evts := append(anthropicTruncationEvents(false), httpx.SSEEvent{
		Event: "error",
		Data:  `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
	})
	last := runAnthropicStream(t, evts)
	if last.Error == nil {
		t.Fatal("an error event must surface on ChatResponse.Error")
	}
	msg := last.Error.Error()
	if !strings.Contains(msg, "overloaded_error") {
		t.Errorf("error %q should name the provider error type", msg)
	}
	if !strings.Contains(msg, "Overloaded") {
		t.Errorf("error %q should carry the provider message", msg)
	}
	if len(last.Content) == 0 {
		t.Error("whatever accumulated before the error should still be delivered")
	}
}

// An error event followed by message_stop is still an error: the provider said
// so explicitly, and "truncated" must not mask it.
func TestAnthropicStreamErrorEventWithMessageStop(t *testing.T) {
	evts := append(anthropicTruncationEvents(true), httpx.SSEEvent{
		Event: "error",
		Data:  `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
	})
	last := runAnthropicStream(t, evts)
	if last.Error == nil {
		t.Fatal("an error event must surface even after message_stop")
	}
	if !strings.Contains(last.Error.Error(), "rate_limit_error") {
		t.Errorf("error = %v, want the provider error type", last.Error)
	}
}

// A malformed error payload must not be swallowed either; the raw data is
// capped so a huge event cannot balloon the error string.
func TestAnthropicStreamUnparseableErrorEvent(t *testing.T) {
	evts := append(anthropicTruncationEvents(false), httpx.SSEEvent{
		Event: "error",
		Data:  strings.Repeat("x", 4096),
	})
	last := runAnthropicStream(t, evts)
	if last.Error == nil {
		t.Fatal("an unparseable error event must still surface")
	}
	msg := last.Error.Error()
	if !strings.Contains(msg, "unparseable") {
		t.Errorf("error = %q, want an unparseable-payload report", msg)
	}
	if len(msg) > 1024 {
		t.Errorf("error message is %d chars; the raw payload should be capped", len(msg))
	}
}
