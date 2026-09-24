package inference

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestWireUsageDisjointCategoriesAndUnknowns(t *testing.T) {
	cases := []struct {
		api, body                  string
		known                      bool
		input, output, read, write int64
	}{
		{"openai-chat", `{"usage":{"prompt_tokens":10,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":3}}}`, true, 7, 4, 3, 0},
		{"xai-chat", `{"usage":{"prompt_tokens":10,"completion_tokens":4,"completion_tokens_details":{"reasoning_tokens":7}}}`, true, 10, 11, 0, 0},
		{"anthropic", `{"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}`, true, 10, 4, 3, 2},
		{"gemini-chat", `{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"cachedContentTokenCount":3,"thoughtsTokenCount":7}}`, true, 7, 11, 3, 0},
		{"openai-embedding", `{"usage":{"total_tokens":3}}`, true, 3, 0, 0, 0},
		{"openai-chat", `{}`, false, 0, 0, 0, 0},
		{"openai-chat", `{"usage":{"prompt_tokens":null,"completion_tokens":0}}`, false, 0, 0, 0, 0},
		{"openai-chat", `{"usage":{"prompt_tokens":-1,"completion_tokens":0}}`, false, 0, 0, 0, 0},
		{"openai-chat", `{"usage":{"prompt_tokens":1,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":2}}}`, false, 1, 0, 0, 0},
	}
	for _, tt := range cases {
		s := decodeUsage(tt.api, []byte(tt.body))
		if s.known() != tt.known || s.usage.InputTokens != tt.input || s.usage.OutputTokens != tt.output || s.usage.CacheReadTokens != tt.read || s.usage.CacheWriteTokens != tt.write {
			t.Errorf("%s %s: %+v", tt.api, tt.body, s)
		}
	}
}
func TestRawStreamTerminalAndAnthropicUsageMerge(t *testing.T) {
	d := testDeployment(t, 1, 0, 0)
	d.cfg.Descriptor.API = "anthropic"
	op := &Operation{deployment: d, stream: &streamAttempt{}}
	op.ObserveStreamData(`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0,"cache_read_input_tokens":4}}}`)
	op.ObserveStreamData(`{"type":"message_delta","usage":{"output_tokens":7}}`)
	if op.StreamError() == nil {
		t.Fatal("usage alone accepted as terminal")
	}
	op.ObserveStreamData(`{"type":"message_stop"}`)
	if op.StreamError() != nil || op.stream.usage.usage.InputTokens != 10 || op.stream.usage.usage.OutputTokens != 7 || op.stream.usage.usage.CacheReadTokens != 4 {
		t.Fatal("Anthropic stream evidence lost")
	}
	op.ObserveStreamData(`{"type":"error","error":{}}`)
	if op.StreamError() == nil {
		t.Fatal("error frame ignored")
	}
	op.stream = &streamAttempt{}
	d.cfg.Descriptor.API = "gemini-chat"
	op.ObserveStreamData(`{"candidates":[{"finishReason":"STOP"}]}`)
	if op.StreamError() != nil {
		t.Fatal("Gemini finish reason missed")
	}
	op.stream = &streamAttempt{}
	op.ObserveStreamData(`{bad`)
	if op.StreamError() == nil {
		t.Fatal("malformed frame accepted")
	}
}
func TestRetryAfterAndManagedClientBoundaries(t *testing.T) {
	for _, s := range []string{"", "bad", "-1", "0"} {
		if parseRetryAfter(s) != 0 {
			t.Fatal(s)
		}
	}
	if parseRetryAfter("2") != 2*time.Second || parseRetryAfter("9999999999999999999999999") <= 0 {
		t.Fatal("Retry-After overflow or parse")
	}
	if delay := parseRetryAfter(time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)); delay < time.Second {
		t.Fatal("HTTP date not honored")
	}
	if _, err := NewHTTPClient(&http.Client{Transport: unsupportedTransport{}}); err == nil {
		t.Fatal("opaque transport accepted")
	}
	transport := &http.Transport{TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{"custom": func(string, *tls.Conn) http.RoundTripper { return unsupportedTransport{} }}}
	if _, err := NewHTTPClient(&http.Client{Transport: transport}); err == nil {
		t.Fatal("opaque protocol handler accepted")
	}
	d := testDeployment(t, 1, 0, 0)
	ctx, finish := d.Start(context.Background(), "test")
	finish(errors.New("fixture"))
	if _, _, _, _, _, err := CurrentOperation(ctx).next(ctx); err == nil {
		t.Fatal("finished operation reused")
	}
}

type unsupportedTransport struct{}

func (unsupportedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("opaque")
}

func TestUsageRejectsReasoningSumOverflow(t *testing.T) {
	for _, tc := range []struct{ api, body string }{
		{"gemini-chat", `{"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":4611686018427387904,"thoughtsTokenCount":4611686018427387904}}`},
		{"xai-chat", `{"usage":{"prompt_tokens":1,"completion_tokens":4611686018427387904,"completion_tokens_details":{"reasoning_tokens":4611686018427387904}}}`},
	} {
		got := decodeUsage(tc.api, []byte(tc.body))
		if got.known() || got.usage.OutputKnown || got.usage.OutputTokens < 0 {
			t.Fatalf("overflow became known usage: %+v", got)
		}
	}
}
