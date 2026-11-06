package main

import (
	"bytes"
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

const response = `{"model":"Piccolo-fixture","answers":{"read_only":{"type":"noul","noul":0.9},"action":{"type":"choice","choice":"read","confidence":0.8,"probabilities":{"read":0.9,"ask":0.1}},"risk":{"type":"score","score":0.1,"confidence":0.8,"legend":{"0":"low","1":"high"},"probabilities":{"0":0.9,"1":0.1}}},"usage":{"input_tokens":12,"output_tokens":0}}`

func TestDefaultFixture(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), false, func(string) string { t.Fatal("offline mode read environment"); return "" }, &output); err != nil {
		t.Fatal(err)
	}
	var got summary
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Evidence != "fixture" || got.Model != "Piccolo-fixture" || got.Action != "read" || got.Attempts != 1 || got.InputTokens == nil || *got.InputTokens != 12 || got.OutputTokens == nil || *got.OutputTokens != 0 {
		t.Fatalf("unexpected fixture output: %s", output.String())
	}
}

func TestLiveFixture(t *testing.T) {
	for _, key := range []string{"", "local-test-key"} {
		t.Run("key="+key, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body struct {
					Model     string
					State     string
					Questions map[string]struct {
						Type     string
						Criteria json.RawMessage
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				expectedKey := key
				if expectedKey == "" {
					expectedKey = "local-systemone"
				}
				if r.Method != "POST" || r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer "+expectedKey || body.Model != "Piccolo-fixture" || !strings.Contains(body.State, "CSV") || len(body.Questions) != 3 {
					t.Error("wrong projected request or credential")
				}
				if body.Questions["read_only"].Type != "noul" || body.Questions["action"].Type != "choice" || body.Questions["risk"].Type != "score" {
					t.Error("missing typed questions")
				}
				var choices map[string]string
				if json.Unmarshal(body.Questions["action"].Criteria, &choices) != nil || len(choices) != 2 {
					t.Error("Piccolo requires at least two choices")
				}
				fmt.Fprint(w, response)
			}))
			defer server.Close()
			env := map[string]string{"SYSTEMONE_LOCAL_ENDPOINT": server.URL + "/v1/systemone", "SYSTEMONE_LOCAL_MODEL": "Piccolo-fixture", "SYSTEMONE_LOCAL_API_KEY": key, "TYPESAFE_API_KEY": "hosted-must-not-leak"}
			var output bytes.Buffer
			if err := run(context.Background(), true, func(k string) string { return env[k] }, &output); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || !strings.Contains(output.String(), `"evidence":"live"`) {
				t.Fatalf("wrong evidence/attempts: %s", output.String())
			}
		})
	}
}

func TestFailuresDoNotPrintDecisions(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"model mismatch", strings.Replace(response, "Piccolo-fixture", "another-model", 1), 200},
		{"missing answers", `{"model":"Piccolo-fixture"}`, 200},
		{"invalid distribution", strings.Replace(response, `"read":0.9`, `"read":2`, 1), 200},
		{"http error", `{"error":"fixture"}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer s.Close()
			env := map[string]string{"SYSTEMONE_LOCAL_ENDPOINT": s.URL, "SYSTEMONE_LOCAL_MODEL": "Piccolo-fixture"}
			var output bytes.Buffer
			if err := run(context.Background(), true, func(k string) string { return env[k] }, &output); err == nil || output.Len() != 0 {
				t.Fatalf("failure accepted: err=%v output=%s", err, output.String())
			}
		})
	}
}

func TestLiveConfigurationRequired(t *testing.T) {
	for _, env := range []map[string]string{
		{}, {"SYSTEMONE_LOCAL_ENDPOINT": "http://127.0.0.1:1"},
		{"SYSTEMONE_LOCAL_ENDPOINT": "http://example.com", "SYSTEMONE_LOCAL_MODEL": "Piccolo"},
		{"SYSTEMONE_LOCAL_ENDPOINT": "http://127.0.0.1:1", "SYSTEMONE_LOCAL_MODEL": "Piccolo", "SYSTEMONE_LOCAL_API_KEY": "bad\nkey"},
	} {
		var output bytes.Buffer
		if err := run(context.Background(), true, func(k string) string { return env[k] }, &output); err == nil || output.Len() != 0 {
			t.Fatal("invalid configuration accepted")
		}
	}
}

func TestCanceled(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); fmt.Fprint(w, response) }))
	defer s.Close()
	env := map[string]string{"SYSTEMONE_LOCAL_ENDPOINT": s.URL, "SYSTEMONE_LOCAL_MODEL": "Piccolo-fixture"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	err := run(ctx, true, func(k string) string { return env[k] }, &output)
	if !errors.Is(err, context.Canceled) || calls.Load() != 0 || output.Len() != 0 {
		t.Fatalf("cancellation lost: %v", err)
	}
}

type failedWriter struct{}

var errWriter = errors.New("fixture writer failed")

func (failedWriter) Write([]byte) (int, error) { return 0, errWriter }
func TestWriterError(t *testing.T) {
	if err := run(context.Background(), false, func(string) string { return "" }, failedWriter{}); !errors.Is(err, errWriter) {
		t.Fatalf("writer error lost: %v", err)
	}
}

func TestUnknownUsage(t *testing.T) {
	body := strings.Replace(response, `,"usage":{"input_tokens":12,"output_tokens":0}`, "", 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
	defer s.Close()
	env := map[string]string{"SYSTEMONE_LOCAL_ENDPOINT": s.URL, "SYSTEMONE_LOCAL_MODEL": "Piccolo-fixture"}
	var output bytes.Buffer
	if err := run(context.Background(), true, func(k string) string { return env[k] }, &output); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["input_tokens"] != nil || got["output_tokens"] != nil {
		t.Fatal("unknown usage became estimated counts")
	}
}

func TestCancelAfterDispatch(t *testing.T) {
	started := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer s.Close()
	env := map[string]string{"SYSTEMONE_LOCAL_ENDPOINT": s.URL, "SYSTEMONE_LOCAL_MODEL": "Piccolo-fixture"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		select {
		case <-started:
			cancel()
		case <-ctx.Done():
		}
	}()
	var output bytes.Buffer
	err := run(ctx, true, func(k string) string { return env[k] }, &output)
	if !errors.Is(err, context.Canceled) || output.Len() != 0 {
		t.Fatalf("in-flight cancellation lost: %v", err)
	}
}
