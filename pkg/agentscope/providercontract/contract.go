// Package providercontract is the provider behavior contract wall
// (HARNESS_DESIGN B1): table-driven contracts that every model provider
// must satisfy, exercised against local httptest fixture servers (zero
// network, zero cost). New or changed providers must pass the wall;
// regressions in usage accounting, streaming lifecycle, error taxonomy, or
// thinking wire formats are caught here instead of in production.
package providercontract

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// Scenario selects which fixture behavior the test server should play.
type Scenario int

const (
	ScnChatSuccess       Scenario = iota // non-stream success with usage
	ScnStreamSuccess                     // full SSE stream with usage
	ScnStreamTruncated                   // SSE stream cut mid-flight
	ScnStreamSlow                        // slow stream (for ctx-cancel)
	ScnErr429                            // rate-limit HTTP error
	ScnErr401                            // auth HTTP error
	ScnCaptureBody                       // record the request body, return success
	ScnCaptureBodyStream                 // record the request body, then play the SSE stream
)

// Harness describes one provider under test: how to construct a model
// pointed at a test server, and how the fixture server should respond in
// each scenario using the provider's own wire format.
type Harness struct {
	Name     string
	NewModel func(baseURL string) (model.ChatModel, error)
	// Serve implements the fixture server behavior for a scenario. It may
	// inspect/record the request (ScnCaptureBody records into captured).
	Serve func(w http.ResponseWriter, r *http.Request, scn Scenario, captured *[]byte)

	ExpectUsage       *model.ChatUsage // expected mapping for ScnChatSuccess
	ExpectStreamText  string           // accumulated text expected from the stream
	ExpectStreamUsage *model.ChatUsage // expected usage on the final stream chunk

	DisableThinkingCheck func(t *testing.T, body []byte) // wire assertion (nil = unsupported)
	EnableThinkingCheck  func(t *testing.T, body []byte)

	// MaxTokensKey is the provider wire key for the output-token limit
	// ("max_tokens", "max_completion_tokens", "maxOutputTokens"). When set,
	// the wall decodes the captured request on both Chat and ChatStream and
	// asserts that model.WithMaxTokens arrives as exactly that number at
	// MaxTokensPath, and that an omitted limit sends neither the key nor the
	// Go field name (agentscope-go#8: the shared OpenAI-compatible struct
	// serialized the field as "MaxTokens"). Empty = check skipped.
	MaxTokensKey string
	// MaxTokensPath is the dot-separated JSON path of the limit in the request
	// body ("generation_config.maxOutputTokens" for Gemini). Empty means the
	// key sits at the top level.
	MaxTokensPath string
}

func newServer(h *Harness, scn Scenario, captured *[]byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Serve(w, r, scn, captured)
	}))
}

func testMsgs() []*message.Msg {
	return []*message.Msg{message.UserMsg("user", "contract test")}
}

// Run executes the full contract wall for one provider harness.
func Run(t *testing.T, h *Harness) {
	t.Helper()

	t.Run("UsageAccounting", func(t *testing.T) {
		srv := newServer(h, ScnChatSuccess, nil)
		defer srv.Close()
		m, err := h.NewModel(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := m.Chat(context.Background(), testMsgs(), model.WithRetries(0, time.Millisecond))
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if resp.Usage == nil {
			t.Fatal("usage missing")
		}
		want := h.ExpectUsage
		if got := resp.Usage; got.InputTokens != want.InputTokens ||
			got.OutputTokens != want.OutputTokens ||
			got.CacheCreationInputTokens != want.CacheCreationInputTokens ||
			got.CacheInputTokens != want.CacheInputTokens {
			t.Errorf("usage = %+v, want %+v", *got, *want)
		}
	})

	t.Run("StreamingLifecycle", func(t *testing.T) {
		srv := newServer(h, ScnStreamSuccess, nil)
		defer srv.Close()
		m, err := h.NewModel(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		ch, err := m.ChatStream(context.Background(), testMsgs())
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		var deltaText strings.Builder
		lastCount := 0
		var lastResp model.ChatResponse
		for resp := range ch {
			if resp.IsLast {
				lastCount++
				lastResp = resp
				continue // final assembled content is checked separately
			}
			for _, b := range resp.Content {
				if tb, ok := b.(message.TextBlock); ok {
					deltaText.WriteString(tb.Text)
				}
			}
		}
		if lastCount != 1 {
			t.Fatalf("IsLast responses = %d, want exactly 1", lastCount)
		}
		if lastResp.Error != nil {
			t.Fatalf("clean stream ended with error: %v", lastResp.Error)
		}
		// Strict equality on deltas: a provider emitting cumulative deltas
		// (double-text bug) must fail the wall (HARNESS review M8).
		if deltaText.String() != h.ExpectStreamText {
			t.Errorf("stream delta text = %q, want exactly %q", deltaText.String(), h.ExpectStreamText)
		}
		// Consistency: the final assembled content must equal the delta
		// accumulation.
		var finalText strings.Builder
		for _, b := range lastResp.Content {
			if tb, ok := b.(message.TextBlock); ok {
				finalText.WriteString(tb.Text)
			}
		}
		if finalText.String() != h.ExpectStreamText {
			t.Errorf("final assembled text = %q, want exactly %q", finalText.String(), h.ExpectStreamText)
		}
		if want := h.ExpectStreamUsage; want != nil {
			got := lastResp.Usage
			if got == nil {
				t.Fatal("final stream chunk missing usage")
			}
			if got.InputTokens != want.InputTokens || got.OutputTokens != want.OutputTokens {
				t.Errorf("stream usage = %+v, want %+v", *got, *want)
			}
		}
	})

	t.Run("StreamTruncationSurfacesError", func(t *testing.T) {
		srv := newServer(h, ScnStreamTruncated, nil)
		defer srv.Close()
		m, err := h.NewModel(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		ch, err := m.ChatStream(context.Background(), testMsgs())
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		var lastResp model.ChatResponse
		sawLast := false
		for resp := range ch {
			if resp.IsLast {
				lastResp = resp
				sawLast = true
			}
		}
		if !sawLast {
			t.Fatal("truncated stream emitted no final IsLast response")
		}
		if lastResp.Error == nil {
			t.Error("truncated stream must surface a final error (silent endings hide data loss)")
		}
	})

	t.Run("CtxCancelStopsStream", func(t *testing.T) {
		srv := newServer(h, ScnStreamSlow, nil)
		defer srv.Close()
		m, err := h.NewModel(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch, err := m.ChatStream(ctx, testMsgs())
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		// Take the first chunk, then abandon.
		if _, ok := <-ch; !ok {
			t.Fatal("expected at least one chunk")
		}
		cancel()
		select {
		case _, ok := <-ch:
			for ok {
				_, ok = <-ch
			}
		case <-time.After(3 * time.Second):
			t.Fatal("stream did not stop after ctx cancel")
		}
	})

	t.Run("ErrorTaxonomy", func(t *testing.T) {
		srv429 := newServer(h, ScnErr429, nil)
		defer srv429.Close()
		m, err := h.NewModel(srv429.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, err = m.Chat(context.Background(), testMsgs(), model.WithRetries(0, time.Millisecond))
		if err == nil {
			t.Fatal("expected error for 429")
		}
		if !model.IsRetryableError(err) {
			t.Errorf("429 must classify as retryable, got: %v", err)
		}

		srv401 := newServer(h, ScnErr401, nil)
		defer srv401.Close()
		m2, err := h.NewModel(srv401.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, err = m2.Chat(context.Background(), testMsgs(), model.WithRetries(0, time.Millisecond))
		if err == nil {
			t.Fatal("expected error for 401")
		}
		if model.IsRetryableError(err) {
			t.Errorf("401 must NOT classify as retryable, got: %v", err)
		}
	})

	if h.DisableThinkingCheck != nil {
		t.Run("ThinkingWireFormat", func(t *testing.T) {
			var captured []byte
			srv := newServer(h, ScnCaptureBody, &captured)
			defer srv.Close()
			m, err := h.NewModel(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Chat(context.Background(), testMsgs(), model.WithThinkingDisabled()); err != nil {
				t.Fatalf("Chat(disabled): %v", err)
			}
			h.DisableThinkingCheck(t, captured)

			captured = nil
			if _, err := m.Chat(context.Background(), testMsgs(), model.WithThinking(true, 1024)); err != nil {
				t.Fatalf("Chat(enabled): %v", err)
			}
			if h.EnableThinkingCheck != nil {
				h.EnableThinkingCheck(t, captured)
			}
		})
	}

	if h.MaxTokensKey != "" {
		t.Run("MaxTokensWireFormat", func(t *testing.T) {
			for _, mode := range []struct {
				name   string
				stream bool
			}{{"chat", false}, {"stream", true}} {
				t.Run(mode.name, func(t *testing.T) {
					scn := ScnCaptureBody
					if mode.stream {
						scn = ScnCaptureBodyStream
					}
					var captured []byte
					srv := newServer(h, scn, &captured)
					defer srv.Close()
					m, err := h.NewModel(srv.URL)
					if err != nil {
						t.Fatal(err)
					}
					call := func(opts ...model.CallOption) {
						t.Helper()
						if !mode.stream {
							if _, err := m.Chat(context.Background(), testMsgs(), opts...); err != nil {
								t.Fatalf("Chat: %v", err)
							}
							return
						}
						ch, err := m.ChatStream(context.Background(), testMsgs(), opts...)
						if err != nil {
							t.Fatalf("ChatStream: %v", err)
						}
						for range ch { // drain: only the captured request matters
						}
					}

					path := h.MaxTokensPath
					if path == "" {
						path = h.MaxTokensKey
					}

					call(model.WithMaxTokens(77))
					// Decode and compare numerically: a substring check for
					// `"max_tokens":77` would also accept 770 or a misplaced
					// field (review of agentscope-go#9).
					got, found, err := jsonPathValue(captured, path)
					switch {
					case err != nil:
						t.Fatalf("request body is not a JSON object: %v; body: %.300s", err, captured)
					case !found:
						t.Errorf("max tokens supplied: %s missing from request; body: %.300s", path, captured)
					default:
						if n, isNum := got.(float64); !isNum || n != 77 {
							t.Errorf("max tokens supplied: %s = %v (%T), want 77", path, got, got)
						}
					}
					requireNotContains(t, captured, `"MaxTokens"`, "max tokens supplied (Go field name must not leak)")

					captured = nil
					call()
					requireNotContains(t, captured, `"`+h.MaxTokensKey+`"`, "max tokens omitted")
					requireNotContains(t, captured, `"MaxTokens"`, "max tokens omitted (Go field name must not leak)")
				})
			}
		})
	}
}

// requireContains fails the test when body does not contain the wire marker.
func requireContains(t *testing.T, body []byte, marker, what string) {
	t.Helper()
	if !strings.Contains(string(body), marker) {
		t.Errorf("%s: request body missing %s; body: %.300s", what, marker, body)
	}
}

// jsonPathValue decodes body as a JSON object and walks the dot-separated
// path through nested objects. found is false when a segment is missing or
// an intermediate value is not an object; err is non-nil when body is not a
// JSON object at all.
func jsonPathValue(body []byte, path string) (value any, found bool, err error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, false, err
	}
	var cur any = root
	for seg := range strings.SplitSeq(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		if cur, ok = obj[seg]; !ok {
			return nil, false, nil
		}
	}
	return cur, true, nil
}

// requireNotContains fails the test when body contains the wire marker.
func requireNotContains(t *testing.T, body []byte, marker, what string) {
	t.Helper()
	if strings.Contains(string(body), marker) {
		t.Errorf("%s: request body must not contain %s; body: %.300s", what, marker, body)
	}
}
