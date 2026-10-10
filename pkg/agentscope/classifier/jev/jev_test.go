package jev_test

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

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/jev"
	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

const fixture = `{"model":"jev-fixture-v1","answers":{"b":{"type":"noul","noul":0.8},"c":{"type":"choice","choice":"yes","confidence":0.6,"probabilities":{"yes":0.75,"no":0.25}},"s":{"type":"score","score":0.75,"confidence":0.5,"legend":{"0":"low","1":"high"},"probabilities":{"0":0.25,"1":0.75}}},"usage":{"input_tokens":12,"output_tokens":0}}`

func request() c.Request {
	return c.Request{OperationID: "op", State: json.RawMessage(`{"text":"任务"}`), Questions: map[string]c.Question{"b": &c.BinaryQuestion{Instructions: "True?", True: "positive", False: "negative"}, "c": &c.ChoiceQuestion{Instructions: "Choose", Choices: map[string]string{"yes": "Y", "no": "N"}}, "s": &c.ScoreQuestion{Instructions: "Rate", Levels: []string{"low", "high"}}}}
}
func client(t *testing.T, endpoint string) *jev.Client {
	t.Helper()
	cl, err := jev.New(jev.Config{APIKey: "fixture-secret", Model: "requested-version", Endpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}
func report(t *testing.T, out *c.Response, err error) {
	t.Helper()
	if out == nil || len(out.Attempts) != 1 || out.Attempts[0].Kind != c.PhysicalAttempt || out.Attempts[0].RequestedModel != "requested-version" || out.Attempts[0].Err != err {
		t.Fatalf("bad report: %+v %v", out, err)
	}
	if e := c.ValidateReport(request(), out); e != nil {
		t.Fatal(e)
	}
}
func TestProtocol(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer fixture-secret" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("wrong HTTP protocol")
		}
		body, _ := io.ReadAll(r.Body)
		var got map[string]any
		if json.Unmarshal(body, &got) != nil {
			t.Error("bad JSON")
		}
		if len(got) != 3 || got["model"] != "requested-version" {
			t.Error("wrong envelope")
		}
		qs := got["questions"].(map[string]any)
		if qs["b"].(map[string]any)["type"] != "noul" || qs["b"].(map[string]any)["criteria"].(map[string]any)["false"] != "negative" || qs["c"].(map[string]any)["criteria"].(map[string]any)["yes"] != "Y" || qs["s"].(map[string]any)["criteria"].([]any)[1] != "high" || got["state"].(map[string]any)["text"] != "任务" {
			t.Error("wrong mapping")
		}
		fmt.Fprint(w, fixture)
	}))
	defer server.Close()
	out, err := client(t, server.URL+"/v1/systemone").Classify(context.Background(), request())
	report(t, out, err)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || out.Model != "jev-fixture-v1" || out.Attempts[0].Model != out.Model || *out.Attempts[0].Usage.InputTokens != 12 || *out.Attempts[0].Usage.OutputTokens != 0 || out.Answers["s"].(*c.ScoreAnswer).Probabilities[1] != 0.75 {
		t.Fatal("lost answers/accounting")
	}
}
func TestHTTPFailuresNoRetries(t *testing.T) {
	for _, status := range []int{400, 401, 403, 422, 429, 500, 529, 302, 307} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/redirect")
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(status)
				fmt.Fprint(w, strings.Replace(fixture, `"answers":`, `"private":"fixture-secret", "answers":`, 1))
			}))
			defer s.Close()
			out, err := client(t, s.URL).Classify(context.Background(), request())
			report(t, out, err)
			var a *ae.AgentError
			var h *jev.HTTPError
			if !errors.As(err, &a) || a.Code != "classifier.failed" || !errors.As(err, &h) || h.StatusCode != status || a.Retryable != (status == 429 || status >= 500) || calls.Load() != 1 || strings.Contains(err.Error(), "fixture-secret") {
				t.Fatalf("wrong failure: %v", err)
			}
			if want := fmt.Sprintf("Jev: HTTP status %d", status); err.Error() != want {
				t.Fatalf("failure message = %q, want %q", err.Error(), want)
			}
			if want := fmt.Sprintf("Jev HTTP status %d", status); h.Error() != want {
				t.Fatalf("HTTP cause message = %q, want %q", h.Error(), want)
			}
			if out.Attempts[0].Usage == nil || *out.Attempts[0].Usage.InputTokens != 12 {
				t.Fatal("lost failure usage")
			}
			if a.Retryable && a.RetryAfter != 2*time.Second {
				t.Fatal("lost retry delay")
			}
		})
	}
}
func TestMalformedResponses(t *testing.T) {
	for name, body := range map[string]string{
		"missing noul": strings.Replace(fixture, `,"noul":0.8`, "", 1), "null noul": strings.Replace(fixture, `"noul":0.8`, `"noul":null`, 1), "bad probability": strings.Replace(fixture, `"noul":0.8`, `"noul":1.1`, 1), "missing confidence": strings.Replace(fixture, `,"confidence":0.6`, "", 1), "null probability": strings.Replace(fixture, `"no":0.25`, `"no":null`, 1), "missing score": strings.Replace(fixture, `,"score":0.75`, "", 1), "bad legend": strings.Replace(fixture, `"0":"low"`, `"0":"other"`, 1), "missing level": strings.Replace(fixture, `"0":0.25,"1":0.75`, `"1":1`, 1), "wrong type": strings.Replace(fixture, `"type":"noul"`, `"type":"choice"`, 1), "bad answers": strings.Replace(fixture, `"noul":0.8`, `"noul":"private"`, 1), "missing model": strings.Replace(fixture, `"model":"jev-fixture-v1",`, "", 1), "trailing": fixture + `{}`, "broken": `{"private":`,
	} {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer s.Close()
			out, err := client(t, s.URL).Classify(context.Background(), request())
			report(t, out, err)
			if !errors.Is(err, c.ErrInvalidResponse) {
				t.Fatalf("accepted %s: %v", name, err)
			}
			if name != "trailing" && name != "broken" && (out.Attempts[0].Usage == nil || *out.Attempts[0].Usage.InputTokens != 12) {
				t.Fatal("bad answer lost valid usage")
			}
		})
	}
}
func TestCancellation(t *testing.T) {
	started := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer s.Close()
	cl := client(t, s.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := cl.Classify(ctx, request())
	if !errors.Is(err, context.Canceled) || len(out.Attempts) != 0 {
		t.Fatal("pre-cancel sent")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); out, err = cl.Classify(ctx, request()) }()
	<-started
	cancel()
	<-done
	report(t, out, err)
	var a *ae.AgentError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &a) || a.Code != "classifier.canceled" || a.Retryable {
		t.Fatal(err)
	}
}
func TestLimits(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, fixture) }))
	defer s.Close()
	for _, response := range []bool{false, true} {
		cfg := jev.Config{APIKey: "k", Model: "requested-version", Endpoint: s.URL}
		if response {
			cfg.MaxResponseBytes = 32
		} else {
			cfg.MaxRequestBytes = 32
		}
		cl, err := jev.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		out, err := cl.Classify(context.Background(), request())
		if response {
			report(t, out, err)
			if !errors.Is(err, c.ErrInvalidResponse) {
				t.Fatal(err)
			}
		} else if !errors.Is(err, c.ErrInvalidRequest) || len(out.Attempts) != 0 || calls.Load() != 0 {
			t.Fatal("oversized request dispatched")
		}
	}
}
