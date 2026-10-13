package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenAITextSingleAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("incorrect destination or auth")
		}
		var wire map[string]any
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Error(err)
		}
		if wire["max_completion_tokens"] != float64(8) || wire["tool_choice"] != "none" || wire["tools"] != nil || wire["stream"] == true {
			t.Errorf("unexpected wire: %v", wire)
		}
		w.WriteHeader(429)
		fmt.Fprint(w, `{"model":"pinned","usage":{"prompt_tokens":7},"error":{"message":"SECRET"}}`)
	}))
	defer srv.Close()
	c, err := NewOpenAITextClient(OpenAIConfig{APIKey: "fixture", Model: "pinned", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Prepare(context.Background(), "approved", 8, 1024)
	if err != nil {
		t.Fatal(err)
	}
	b := p.JSON()
	b[0] = '!'
	if !json.Valid(p.JSON()) {
		t.Fatal("mutable prepared body")
	}
	dispatched := 0
	res, err := c.Send(context.Background(), p, 1024, func() error { dispatched++; return nil })
	if err == nil || strings.Contains(err.Error(), "SECRET") || calls.Load() != 1 || dispatched != 1 {
		t.Fatalf("single safe attempt: %v %d %d", err, calls.Load(), dispatched)
	}
	if res.Usage == nil || res.Usage.InputTokens == nil || *res.Usage.InputTokens != 7 || res.Usage.OutputTokens != nil {
		t.Fatalf("lost partial usage: %+v", res.Usage)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Send(ctx, p, 1024, nil)
	if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatal("canceled call dispatched")
	}
}

func TestOpenAITextResponseValidation(t *testing.T) {
	cases := []struct {
		name, body string
		want       error
		reason     string
	}{
		{"valid", `{"model":"pinned","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":0,"completion_tokens":2}}`, nil, "stop"},
		{"bad json", `{`, ErrTextInvalidResponse, ""},
		{"no model", `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`, ErrTextInvalidResponse, ""},
		{"empty choices", `{"model":"pinned","choices":[]}`, ErrTextInvalidResponse, ""},
		{"many choices", `{"model":"pinned","choices":[{},{}]}`, ErrTextInvalidResponse, ""},
		{"missing index", `{"model":"pinned","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`, ErrTextInvalidResponse, ""},
		{"wrong index", `{"model":"pinned","choices":[{"index":1,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`, ErrTextInvalidResponse, ""},
		{"wrong role", `{"model":"pinned","choices":[{"index":0,"finish_reason":"stop","message":{"role":"user","content":"ok"}}]}`, ErrTextInvalidResponse, ""},
		{"content object", `{"model":"pinned","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":{"text":"ok"}}}]}`, ErrTextInvalidResponse, ""},
		{"empty content", `{"model":"pinned","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":null}}]}`, ErrTextInvalidResponse, ""},
		{"missing finish", `{"model":"pinned","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`, ErrTextInvalidResponse, ""},
		{"length", `{"model":"pinned","choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":"partial"}}]}`, ErrTextIncomplete, "length"},
		{"filter", `{"model":"pinned","choices":[{"index":0,"finish_reason":"content_filter","message":{"role":"assistant","content":"partial"}}]}`, ErrTextIncomplete, "content_filter"},
		{"refusal", `{"model":"pinned","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"partial","refusal":"denied"}}]}`, ErrTextIncomplete, "content_filter"},
		{"tool even stop", `{"model":"pinned","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok","tool_calls":[{}]}}]}`, ErrTextUnsupported, ""},
		{"tool finish", `{"model":"pinned","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"ok"}}]}`, ErrTextUnsupported, ""},
		{"legacy tool", `{"model":"pinned","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok","function_call":{}}}]}`, ErrTextUnsupported, ""},
		{"invalid tool", `{"model":"pinned","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok","tool_calls":{}}}]}`, ErrTextInvalidResponse, ""},
		{"audio", `{"model":"pinned","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok","audio":{}}}]}`, ErrTextUnsupported, ""},
		{"error envelope", `{"model":"pinned","error":{"message":"SECRET"},"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`, ErrTextInvalidResponse, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			c, err := NewOpenAITextClient(OpenAIConfig{APIKey: "fixture", Model: "pinned", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseIdleConnections()
			p, err := c.Prepare(context.Background(), "text", 10, 1024)
			if err != nil {
				t.Fatal(err)
			}
			r, err := c.Send(context.Background(), p, 4096, nil)
			if !errors.Is(err, tc.want) || r.FinishReason != tc.reason {
				t.Fatalf("result=%+v error=%v", r, err)
			}
			if tc.want != nil && r.Text != "" {
				t.Fatal("failed response published text")
			}
			if tc.name == "valid" && (r.Text != "ok" || r.Usage == nil || r.Usage.InputTokens == nil || *r.Usage.InputTokens != 0) {
				t.Fatalf("lost valid zero: %+v", r)
			}
		})
	}
}

type textOpaqueTransport struct{}

func (textOpaqueTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("must reject opaque transport")
}

func TestOpenAITextConfigAndPrepare(t *testing.T) {
	for _, cfg := range []OpenAIConfig{
		{}, {APIKey: "x"}, {APIKey: "x", Model: "m", BaseURL: "ftp://invalid"}, {APIKey: "x", Model: "m", BaseURL: "https://u:p@host"}, {APIKey: "x", Model: "m", BaseURL: "https://host?q=x"}, {APIKey: "x", Model: "m", BaseURL: "https://host#fragment"},
		{APIKey: "x", Model: "m", HTTPClient: &http.Client{Transport: textOpaqueTransport{}}},
		{APIKey: "x", Model: "m", ClientOptions: &ClientOptions{DefaultHeaders: map[string]string{"Idempotency-Key": "x"}}},
	} {
		if _, err := NewOpenAITextClient(cfg); !errors.Is(err, ErrTextInvalidRequest) {
			t.Fatalf("config accepted: %+v", cfg)
		}
	}
	c, err := NewOpenAITextClient(OpenAIConfig{APIKey: "x", Model: "m", ClientOptions: &ClientOptions{DefaultHeaders: map[string]string{"authorization": "bad", "Content-Type": "bad", "X-Fixture": "yes"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	endpoint, name := c.Target()
	if endpoint != "https://api.openai.com/v1/chat/completions" || name != "m" {
		t.Fatal("wrong binding")
	}
	for _, tc := range []struct {
		text          string
		output, limit int
	}{{"", 1, 100}, {"x", 0, 100}, {"x", 1, 0}, {"long", 1, 2}, {"x", 1, 5}, {string([]byte{0xff}), 1, 100}} {
		if _, err = c.Prepare(context.Background(), tc.text, tc.output, tc.limit); !errors.Is(err, ErrTextInvalidRequest) {
			t.Fatal("invalid request accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = c.Prepare(ctx, "x", 1, 100); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = c.Send(context.Background(), nil, 100, nil); !errors.Is(err, ErrTextInvalidRequest) {
		t.Fatal(err)
	}
	other, _ := NewOpenAITextClient(OpenAIConfig{APIKey: "x", Model: "m"})
	defer other.CloseIdleConnections()
	p, _ := other.Prepare(context.Background(), "x", 1, 1024)
	if _, err = c.Send(context.Background(), p, 100, nil); !errors.Is(err, ErrTextInvalidRequest) {
		t.Fatal("cross-client request accepted")
	}
}

func TestOpenAITextRedirectLimitAndReadFailure(t *testing.T) {
	var destination atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destination.Add(1) }))
	defer sink.Close()
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		limit   int
	}{
		{"redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, sink.URL, 307) }, 4096},
		{"oversize", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, strings.Repeat("x", 20)) }, 10},
		{"short body", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			fmt.Fprint(w, `{"usage":{}}`)
		}, 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c, err := NewOpenAITextClient(OpenAIConfig{APIKey: "fixture", Model: "pinned", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseIdleConnections()
			p, _ := c.Prepare(context.Background(), "text", 1, 1024)
			r, err := c.Send(context.Background(), p, tc.limit, nil)
			if err == nil || r.Text != "" || r.Usage != nil || destination.Load() != 0 {
				t.Fatalf("unsafe result: %+v %v", r, err)
			}
		})
	}
}

func TestOpenAITextDispatchAndCancellation(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); close(started); <-release }))
	defer srv.Close()
	defer close(release)
	c, err := NewOpenAITextClient(OpenAIConfig{APIKey: "fixture", Model: "pinned", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	p, _ := c.Prepare(context.Background(), "text", 1, 1024)
	_, err = c.Send(context.Background(), p, 1024, func() error { return errors.New("SECRET") })
	if err == nil || strings.Contains(err.Error(), "SECRET") || calls.Load() != 0 {
		t.Fatal("begin rejection leaked/dispatched")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.Send(ctx, p, 1024, nil); done <- err }()
	<-started
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("cancel: %v", err)
	}
}

func TestTextUsageAndRetryAfter(t *testing.T) {
	u := readTextUsage(json.RawMessage(`{"prompt_tokens":4,"completion_tokens":-1,"prompt_tokens_details":{"cached_tokens":2}}`))
	if u.InputTokens == nil || *u.InputTokens != 4 || u.OutputTokens != nil || u.CachedInputTokens == nil || *u.CachedInputTokens != 2 {
		t.Fatalf("partial usage: %+v", u)
	}
	u = readTextUsage(json.RawMessage(`{"prompt_tokens":4,"completion_tokens":"bad","prompt_tokens_details":{"cached_tokens":9}}`))
	if u.CachedInputTokens != nil || u.OutputTokens != nil {
		t.Fatal("invalid usage accepted")
	}
	for _, raw := range []string{`null`, `[]`, `broken`} {
		if readTextUsage(json.RawMessage(raw)) != nil {
			t.Fatal("unknown usage invented")
		}
	}
	if textRetryAfter("2") != 2*time.Second || textRetryAfter("9223372036854775807") != 0 || textRetryAfter("nonsense") != 0 || textRetryAfter("0") != 0 {
		t.Fatal("bad delay")
	}
	if textRetryAfter(time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)) <= 0 || textRetryAfter(time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)) != 0 {
		t.Fatal("bad date delay")
	}
}

func TestOpenAITextForcesHTTP1AndSnapshotsClient(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.ProtoMajor != 1 {
			t.Errorf("unexpected replay-capable protocol: %s", r.Proto)
		}
		if r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("X-Fixture") != "original" || r.Header.Get("Cookie") != "" {
			t.Error("headers/config were not snapshotted")
		}
		fmt.Fprint(w, `{"model":"pinned","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	original := srv.Client()
	original.Timeout = time.Second
	tr := original.Transport.(*http.Transport)
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetHTTP1(true)
	tr.Protocols.SetHTTP2(true)
	headers := map[string]string{"X-Fixture": "original", "authorization": "bad"}
	c, err := NewOpenAITextClient(OpenAIConfig{APIKey: "fixture", Model: "pinned", BaseURL: srv.URL, HTTPClient: original, ClientOptions: &ClientOptions{DefaultHeaders: headers}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	headers["X-Fixture"] = "mutated"
	if original.Timeout != time.Second || c.client.Timeout != 0 || c.client.Transport == original.Transport {
		t.Fatal("original client mutated/shared")
	}
	p, err := c.Prepare(context.Background(), "x", 1, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		r, err := c.Send(context.Background(), p, 1024, nil)
		if err != nil || r.Text != "ok" {
			t.Fatalf("send: %+v %v", r, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("hidden attempts")
	}
}

func TestOpenAITextDoesNotReplayConnectionFailure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()
	c, err := NewOpenAITextClient(OpenAIConfig{APIKey: "fixture", Model: "pinned", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	p, _ := c.Prepare(context.Background(), "x", 1, 1024)
	_, err = c.Send(context.Background(), p, 1024, nil)
	if err == nil || calls.Load() != 1 {
		t.Fatalf("replayed failed POST: %v calls=%d", err, calls.Load())
	}
}

func TestOpenAITextInvalidUTF8DoesNotInventUsage(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			// encoding/json replaces invalid UTF8 in a JSON string rather than failing.
			// The strict primitive must reject the representation before trusting usage.
			body := strings.ReplaceAll(`{"model":"pinned","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"BAD"}}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`, "BAD", string([]byte{0xff}))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status); fmt.Fprint(w, body) }))
			defer srv.Close()
			c, err := NewOpenAITextClient(OpenAIConfig{APIKey: "fixture", Model: "pinned", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseIdleConnections()
			p, _ := c.Prepare(context.Background(), "approved", 1, 1024)
			result, err := c.Send(context.Background(), p, 4096, nil)
			if err == nil || result.Text != "" || result.Usage != nil || result.Model != "" {
				t.Fatalf("invalid representation trusted: %+v %v", result, err)
			}
			if status == http.StatusTooManyRequests {
				var httpErr *TextHTTPError
				if !errors.As(err, &httpErr) || httpErr.StatusCode != status {
					t.Fatalf("lost structured HTTP status: %v", err)
				}
			}
		})
	}
}
