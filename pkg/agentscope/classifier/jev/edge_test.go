package jev_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/jev"
	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestConfigAndPreDispatch(t *testing.T) {
	for _, cfg := range []jev.Config{{}, {APIKey: "k"}, {APIKey: "k\n", Model: "m"}, {APIKey: "k", Model: "m", Endpoint: "ftp://host"}, {APIKey: "k", Model: "m", Endpoint: "https://user:pass@host"}, {APIKey: "k", Model: "m", Endpoint: "https://host#fragment"}, {APIKey: "k", Model: "m", Endpoint: "https://host?secret=x"}, {APIKey: "k", Model: "m", Endpoint: "://"}, {APIKey: "k", Model: "m", Endpoint: "https:///path"}, {APIKey: "k", Model: "m", MaxRequestBytes: -1}, {APIKey: "k", Model: "m", MaxResponseBytes: -1}, {APIKey: "k", Model: "m", HTTPClient: &http.Client{Timeout: time.Second}}} {
		if _, err := jev.New(cfg); !errors.Is(err, c.ErrInvalidRequest) {
			t.Fatalf("bad config accepted: %v", err)
		}
	}
	cl, err := jev.New(jev.Config{APIKey: "k", Model: "m", HTTPClient: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		t.Error("unexpected dispatch")
		return nil, errors.New("dispatched")
	})}})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*c.Request){"invalid": func(r *c.Request) { r.OperationID = "" }, "one score": func(r *c.Request) { r.Questions["s"] = &c.ScoreQuestion{Instructions: "rate", Levels: []string{"one"}} }, "many scores": func(r *c.Request) {
		ls := []string{}
		for i := 0; i < 11; i++ {
			ls = append(ls, fmt.Sprint(i))
		}
		r.Questions["s"] = &c.ScoreQuestion{Instructions: "rate", Levels: ls}
	}, "many choices": func(r *c.Request) {
		choices := map[string]string{}
		for i := 0; i < 256; i++ {
			choices[fmt.Sprint(i)] = ""
		}
		r.Questions["c"] = &c.ChoiceQuestion{Instructions: "choose", Choices: choices}
	}} {
		t.Run(name, func(t *testing.T) {
			r := request()
			mutate(&r)
			out, e := cl.Classify(context.Background(), r)
			if !errors.Is(e, c.ErrInvalidRequest) || len(out.Attempts) != 0 {
				t.Fatal(e)
			}
		})
	}
	var missingContext context.Context // Deliberately exercise invalid caller input.
	out, err := cl.Classify(missingContext, request())
	if !errors.Is(err, c.ErrInvalidRequest) || len(out.Attempts) != 0 {
		t.Fatal(err)
	}
	for _, cl := range []*jev.Client{nil, {}} {
		out, err = cl.Classify(context.Background(), request())
		if !errors.Is(err, c.ErrInvalidRequest) || len(out.Attempts) != 0 {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	out, err = cl.Classify(ctx, request())
	if !errors.Is(err, context.DeadlineExceeded) || len(out.Attempts) != 0 {
		t.Fatal(err)
	}
}
func TestUsage(t *testing.T) {
	for _, tc := range []struct {
		name, usage   string
		input, output bool
		invalid       bool
	}{{"missing", "", false, false, false}, {"null", `,"usage":null`, false, false, false}, {"partial", `,"usage":{"input_tokens":0}`, true, false, false}, {"invalid input", `,"usage":{"input_tokens":-1,"output_tokens":0}`, false, true, true}, {"invalid output", `,"usage":{"input_tokens":0,"output_tokens":"bad"}`, true, false, true}, {"invalid usage", `,"usage":[]`, false, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(fixture, `,"usage":{"input_tokens":12,"output_tokens":0}`, tc.usage, 1)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer s.Close()
			out, err := client(t, s.URL).Classify(context.Background(), request())
			report(t, out, err)
			if (err != nil) != tc.invalid {
				t.Fatal(err)
			}
			u := out.Attempts[0].Usage
			if (u != nil && u.InputTokens != nil) != tc.input || (u != nil && u.OutputTokens != nil) != tc.output {
				t.Fatalf("wrong unknown counts: %+v", u)
			}
		})
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("private-body") }
func (errorReader) Close() error             { return nil }
func TestTransportAndLateCancellation(t *testing.T) {
	for _, mode := range []string{"network", "read", "wrapped deadline", "late cancellation"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			transport := roundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.GetBody != nil {
					t.Error("replay enabled")
				}
				switch mode {
				case "network":
					return nil, errors.New("fixture-secret private-body")
				case "wrapped deadline":
					return nil, fmt.Errorf("private-body: %w", context.DeadlineExceeded)
				case "read":
					return &http.Response{StatusCode: 200, Body: errorReader{}, Header: make(http.Header)}, nil
				default:
					cancel()
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fixture)), Header: make(http.Header)}, nil
				}
			})
			hc := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { t.Error("caller redirect policy invoked"); return nil }}
			cl, err := jev.New(jev.Config{APIKey: "k", Model: "requested-version", HTTPClient: hc})
			if err != nil {
				t.Fatal(err)
			}
			out, err := cl.Classify(ctx, request())
			report(t, out, err)
			if calls != 1 || err == nil {
				t.Fatal(err)
			}
			for e := err; e != nil; e = errors.Unwrap(e) {
				if strings.Contains(e.Error(), "private-body") || strings.Contains(e.Error(), "fixture-secret") {
					t.Fatal("leaked transport error")
				}
			}
			var a *ae.AgentError
			if !errors.As(err, &a) {
				t.Fatal(err)
			}
			if mode == "wrapped deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if mode == "late cancellation" {
				if !errors.Is(err, context.Canceled) || out.Attempts[0].Usage == nil {
					t.Fatal("late cancellation lost usage")
				}
			}
			if hc.CheckRedirect == nil {
				t.Fatal("mutated custom client")
			}
		})
	}
}
func TestConcurrentAndOptionalCriteria(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"criteria"`) {
			t.Error("empty Noul criteria sent")
		}
		fmt.Fprint(w, `{"model":"fixture","answers":{"b":{"type":"noul","noul":0}}}`)
	}))
	defer s.Close()
	cl := client(t, s.URL)
	r := request()
	r.Questions = map[string]c.Question{"b": &c.BinaryQuestion{Instructions: "True?"}}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := cl.Classify(context.Background(), r)
			if err != nil || out.Answers["b"].(*c.BinaryAnswer).Probability != 0 {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestRedirectDoesNotFollowOrMutateClient(t *testing.T) {
	var targetCalls, callbackCalls int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++; fmt.Fprint(w, fixture) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { callbackCalls++; return nil }}
	cl, err := jev.New(jev.Config{APIKey: "k", Model: "requested-version", Endpoint: source.URL, HTTPClient: hc})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cl.Classify(context.Background(), request())
	report(t, out, err)
	if err == nil || targetCalls != 0 || callbackCalls != 0 {
		t.Fatal("redirect followed")
	}
	// Calling the original client still follows its policy.
	resp, err := hc.Get(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if targetCalls != 1 || callbackCalls != 1 {
		t.Fatal("caller client modified")
	}
}
func TestDeadlineDuringResponseRead(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	out, err := client(t, s.URL).Classify(ctx, request())
	report(t, out, err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
func TestRetryAfterHints(t *testing.T) {
	for _, tc := range []struct {
		header   string
		positive bool
	}{{"0", false}, {"-1", false}, {"99999999999999999", false}, {"invalid", false}, {"", false}, {time.Now().Add(time.Hour).UTC().Format(http.TimeFormat), true}, {time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), false}} {
		t.Run(tc.header, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", tc.header)
				w.WriteHeader(529)
			}))
			defer s.Close()
			_, err := client(t, s.URL).Classify(context.Background(), request())
			var a *ae.AgentError
			if !errors.As(err, &a) || !a.Retryable || (a.RetryAfter > 0) != tc.positive {
				t.Fatal(err)
			}
		})
	}
}
