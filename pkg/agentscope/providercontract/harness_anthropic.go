package providercontract

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

const anthropicSuccessBody = `{
	"id": "msg_c1", "type": "message", "role": "assistant", "model": "claude-test",
	"content": [{"type": "text", "text": "hello contract"}],
	"stop_reason": "end_turn",
	"usage": {"input_tokens": 10, "output_tokens": 5,
		"cache_creation_input_tokens": 2, "cache_read_input_tokens": 3}
}`

// Real Anthropic SSE carries an event: line per data: line; the Go stream
// processor dispatches on the event type, so fixtures must include both.
const anthropicStreamBody = `event: message_start
data: {"type":"message_start","message":{"id":"msg_s1","type":"message","role":"assistant","model":"claude-test","usage":{"input_tokens":8}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello contract"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}

event: message_stop
data: {"type":"message_stop"}

`

func anthropicServe(w http.ResponseWriter, r *http.Request, scn Scenario, captured *[]byte) {
	switch scn {
	case ScnChatSuccess, ScnCaptureBody:
		if captured != nil {
			// io.ReadAll: single Read short-reads and ContentLength == -1
			// (chunked) would corrupt the captured wire body (HARNESS
			// review M8).
			buf, _ := io.ReadAll(r.Body)
			*captured = buf
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, anthropicSuccessBody)
	case ScnCaptureBodyStream:
		if captured != nil {
			buf, _ := io.ReadAll(r.Body)
			*captured = buf
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, anthropicStreamBody)
	case ScnStreamSuccess:
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, anthropicStreamBody)
	case ScnStreamTruncated:
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":8}}}\n\n")
		fl.Flush()
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	case ScnStreamSlow:
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"slow\"}}\n\n")
		fl.Flush()
		time.Sleep(2 * time.Second)
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		fl.Flush()
	case ScnErr429:
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`)
	case ScnErr401:
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`)
	}
}

// AnthropicHarness returns the contract harness for Anthropic, including
// thinking wire-format assertions ({"thinking":{"type":"disabled"}} vs
// enabled+budget).
func AnthropicHarness() Harness {
	return Harness{
		Name: "anthropic",
		NewModel: func(baseURL string) (model.ChatModel, error) {
			return model.NewAnthropicChatModel(&model.AnthropicConfig{
				APIKey: "test-key", Model: "claude-test", BaseURL: baseURL,
			})
		},
		Serve:             anthropicServe,
		ExpectUsage:       &model.ChatUsage{InputTokens: 10, OutputTokens: 5, CacheCreationInputTokens: 2, CacheInputTokens: 3},
		ExpectStreamText:  "hello contract",
		ExpectStreamUsage: &model.ChatUsage{InputTokens: 8, OutputTokens: 4},
		// MaxTokensKey intentionally unset: the Anthropic adapter always sends
		// its configured default max_tokens and does not yet apply
		// model.WithMaxTokens per call (a separate gap from agentscope-go#8,
		// which covers the shared OpenAI-compatible request struct).
		DisableThinkingCheck: func(t *testing.T, body []byte) {
			requireContains(t, body, `"thinking":{"type":"disabled"}`, "anthropic disable-thinking wire format")
		},
		EnableThinkingCheck: func(t *testing.T, body []byte) {
			requireContains(t, body, `"type":"enabled"`, "anthropic enable-thinking wire format")
			requireContains(t, body, `"budget_tokens":1024`, "anthropic thinking budget")
		},
	}
}
