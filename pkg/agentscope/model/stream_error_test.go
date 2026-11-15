package model

import (
	"context"
	"errors"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
)

func drainStream(sseCh chan httpx.SSEEvent) []ChatResponse {
	outCh := make(chan ChatResponse, 32)
	processOpenAIStream(context.Background(), sseCh, outCh)
	var out []ChatResponse
	for r := range outCh {
		out = append(out, r)
	}
	return out
}

func TestProcessOpenAIStreamCompletionProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, suffix string
		wantError    bool
	}{
		{"clean_eof", "", true},
		{"malformed", "{bad", true},
		{"done", "[DONE]", false},
		{"finish_reason", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := make(chan httpx.SSEEvent, 2)
			in <- httpx.SSEEvent{Data: `{"choices":[{"delta":{"content":"partial"}}]}`}
			if tc.suffix != "" {
				in <- httpx.SSEEvent{Data: tc.suffix}
			}
			close(in)
			out := drainStream(in)
			last := out[len(out)-1]
			if !last.IsLast || (last.Error != nil) != tc.wantError {
				t.Fatalf("final=%+v", last)
			}
		})
	}
}

// TestProcessOpenAIStream_PropagatesTerminalError proves a mid-stream transport
// failure (surfaced as SSEEvent.Err) becomes a ChatResponse with Error set,
// instead of silently ending as a normal, truncated IsLast response.
func TestProcessOpenAIStream_PropagatesTerminalError(t *testing.T) {
	sseCh := make(chan httpx.SSEEvent, 4)
	sseCh <- httpx.SSEEvent{Data: `{"id":"x","choices":[{"delta":{"content":"partial"}}]}`}
	sseCh <- httpx.SSEEvent{Err: errors.New("connection reset by peer")}
	close(sseCh)

	got := drainStream(sseCh)
	if len(got) == 0 {
		t.Fatal("expected at least one response")
	}
	last := got[len(got)-1]
	if last.Error == nil {
		t.Fatal("terminal stream error was swallowed; expected ChatResponse.Error to be set")
	}
	if !last.IsLast {
		t.Error("error response should be marked IsLast")
	}
}

// TestProcessOpenAIStream_SurfacesStopReason proves finish_reason is normalized
// onto StopReason (so callers can detect length/content_filter truncation).
func TestProcessOpenAIStream_SurfacesStopReason(t *testing.T) {
	sseCh := make(chan httpx.SSEEvent, 4)
	sseCh <- httpx.SSEEvent{Data: `{"id":"x","choices":[{"delta":{"content":"hi"},"finish_reason":"length"}]}`}
	sseCh <- httpx.SSEEvent{Data: "[DONE]"}
	close(sseCh)

	got := drainStream(sseCh)
	last := got[len(got)-1]
	if last.StopReason != StopReasonLength {
		t.Fatalf("StopReason = %q, want %q", last.StopReason, StopReasonLength)
	}
}

// TestProcessOpenAIStream_TrailingUsageOnlyChunk locks in the fix for upstream
// issue #2314 (Moonshot streaming usage): providers such as Moonshot emit a
// trailing chunk with usage but no choices. The accumulated usage must still
// surface on the final IsLast response.
func TestProcessOpenAIStream_TrailingUsageOnlyChunk(t *testing.T) {
	sseCh := make(chan httpx.SSEEvent, 4)
	sseCh <- httpx.SSEEvent{Data: `{"id":"x","choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`}
	// Trailing usage-only chunk: no choices.
	sseCh <- httpx.SSEEvent{Data: `{"id":"x","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`}
	sseCh <- httpx.SSEEvent{Data: "[DONE]"}
	close(sseCh)

	got := drainStream(sseCh)
	if len(got) == 0 {
		t.Fatal("expected at least one response")
	}
	last := got[len(got)-1]
	if !last.IsLast {
		t.Fatal("last response should be marked IsLast")
	}
	if last.Usage == nil {
		t.Fatal("usage from trailing usage-only chunk was dropped")
	}
	if last.Usage.InputTokens != 11 || last.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v, want input 11 / output 7", last.Usage)
	}
}
