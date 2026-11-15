package bailian_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/bailian"
	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

// Synthetic responses follow the documented System One schema, not a live run.
const fixture = `{"model":"decision-model-preview","request_id":"fixture-id","answers":{"binary":{"type":"noul","noul":0.8},"choice":{"type":"choice","choice":"billing","confidence":0.6,"probabilities":{"billing":0.75,"technical":0.25}},"score":{"type":"score","score":0.75,"confidence":0.5,"legend":{"0":"low","1":"high"},"probabilities":{"0":0.25,"1":0.75}}},"usage":{"input_tokens":12},"latency_ms":1.2}`

func request() c.Request {
	return c.Request{
		OperationID: "ticket-1", State: json.RawMessage(`{"text":"支付失败"}`),
		Questions: map[string]c.Question{
			"binary": &c.BinaryQuestion{Instructions: "Escalate?", True: "urgent", False: "routine"},
			"choice": &c.ChoiceQuestion{Instructions: "Which team?", Choices: map[string]string{"billing": "Payments", "technical": "Software"}},
			"score":  &c.ScoreQuestion{Instructions: "Severity?", Levels: []string{"low", "high"}},
		},
	}
}

func config(endpoint string) bailian.Config {
	return bailian.Config{APIKey: "fixture-secret", Model: "decision-model-preview", Endpoint: endpoint}
}

func client(t *testing.T, cfg bailian.Config) *bailian.Client {
	t.Helper()
	cl, err := bailian.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

func checkReport(t *testing.T, r c.Request, out *c.Response, err error, attempts int) {
	t.Helper()
	if e := c.ValidateReport(r, out); e != nil {
		t.Fatal(e)
	}
	if len(out.Attempts) != attempts {
		t.Fatalf("attempts=%d, want %d", len(out.Attempts), attempts)
	}
	if attempts == 1 {
		a := out.Attempts[0]
		if a.Kind != c.PhysicalAttempt || a.RequestedModel != "decision-model-preview" || a.Err != err {
			t.Fatalf("incorrect attempt: %+v, err=%v", a, err)
		}
	}
}

func TestProtocol(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/compatible-mode/v1/systemone" || r.Header.Get("Authorization") != "Bearer fixture-secret" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
			t.Error("incorrect HTTP method/path/auth/headers")
		}
		var got struct {
			Model     string            `json:"model"`
			State     map[string]string `json:"state"`
			Questions map[string]struct {
				Type         string          `json:"type"`
				Instructions string          `json:"instructions"`
				Criteria     json.RawMessage `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if got.Model != "decision-model-preview" || got.State["text"] != "支付失败" || len(got.Questions) != 3 {
			t.Error("incorrect request envelope")
		}
		for id, expected := range map[string]string{"binary": "noul", "choice": "choice", "score": "score"} {
			if got.Questions[id].Type != expected || got.Questions[id].Instructions == "" {
				t.Errorf("incorrect question %s", id)
			}
		}
		var binary, choice map[string]string
		var score []string
		if json.Unmarshal(got.Questions["binary"].Criteria, &binary) != nil || json.Unmarshal(got.Questions["choice"].Criteria, &choice) != nil || json.Unmarshal(got.Questions["score"].Criteria, &score) != nil || !reflect.DeepEqual(binary, map[string]string{"true": "urgent", "false": "routine"}) || !reflect.DeepEqual(choice, map[string]string{"billing": "Payments", "technical": "Software"}) || !reflect.DeepEqual(score, []string{"low", "high"}) {
			t.Error("incorrect criteria")
		}
		fmt.Fprint(w, fixture)
	}))
	defer s.Close()
	r := request()
	before, _ := json.Marshal(r)
	cl := client(t, config(s.URL+"/compatible-mode/v1/systemone"))
	out, err := cl.Classify(context.Background(), r)
	checkReport(t, r, out, err, 1)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(r)
	if !bytes.Equal(before, after) || calls.Load() != 1 || out.Model != "decision-model-preview" || out.Attempts[0].Model != out.Model || *out.Attempts[0].Usage.InputTokens != 12 || out.Attempts[0].Usage.OutputTokens != nil {
		t.Fatal("lost accounting or mutated request")
	}
	if out.Answers["binary"].(*c.BinaryAnswer).Probability != 0.8 || out.Answers["choice"].(*c.ChoiceAnswer).Confidence != 0.6 || out.Answers["score"].(*c.ScoreAnswer).Probabilities[1] != 0.75 {
		t.Fatal("lost typed answers")
	}
}

func levels(n int) []string {
	ls := make([]string, n)
	for i := range ls {
		ls[i] = fmt.Sprintf("level-%d", i)
	}
	return ls
}

func TestProviderLimits(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		questions, choices, scores int
		valid                      bool
	}{
		{"minimum", 2, 1, 2, true}, {"eleven score levels", 2, 1, 11, true},
		{"maximum", 16, 255, 255, true}, {"too many questions", 17, 1, 2, false},
		{"too many choices", 1, 256, 2, false}, {"one score level", 1, 1, 1, false},
		{"too many scores", 1, 1, 256, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			choices := map[string]string{}
			for i := 0; i < tc.choices; i++ {
				choices[fmt.Sprint(i)] = ""
			}
			r := request()
			r.Questions = map[string]c.Question{"choice": &c.ChoiceQuestion{Instructions: "team", Choices: choices}, "score": &c.ScoreQuestion{Instructions: "rate", Levels: levels(tc.scores)}}
			// Include the choice and score in the total question count.
			for i := 2; i < tc.questions; i++ {
				r.Questions[fmt.Sprint(i)] = &c.BinaryQuestion{Instructions: "true?"}
			}
			guardCalls := 0
			stop := errors.New("prepared successfully")
			cl := client(t, config("https://fixture.invalid/compatible-mode/v1/systemone"))
			out, err := cl.ClassifyWithDispatch(context.Background(), r, c.DispatchLimits{}, func(kind c.AttemptKind) error {
				guardCalls++
				if kind != c.PhysicalAttempt {
					t.Error("wrong attempt kind")
				}
				return stop
			})
			checkReport(t, r, out, err, 0)
			if tc.valid {
				if err != stop || guardCalls != 1 {
					t.Fatalf("legal request rejected: %v", err)
				}
			} else if !errors.Is(err, c.ErrInvalidRequest) || guardCalls != 0 {
				t.Fatalf("invalid request reached guard: %v", err)
			}
		})
	}
}

func TestWideScoreResponse(t *testing.T) {
	for _, n := range []int{11, 255} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			ls := levels(n)
			legend := map[string]string{}
			ps := map[string]float64{}
			for i, level := range ls {
				key := fmt.Sprint(i)
				legend[key] = level
				ps[key] = 0
			}
			ps[fmt.Sprint(n-1)] = 1
			body, _ := json.Marshal(map[string]any{"model": "decision-model-preview", "answers": map[string]any{"score": map[string]any{"type": "score", "score": n - 1, "confidence": 0.7, "legend": legend, "probabilities": ps}}})
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, string(body)) }))
			defer s.Close()
			r := request()
			r.Questions = map[string]c.Question{"score": &c.ScoreQuestion{Instructions: "rate", Levels: ls}}
			out, err := client(t, config(s.URL)).Classify(context.Background(), r)
			checkReport(t, r, out, err, 1)
			if err != nil {
				t.Fatal(err)
			}
			if out.Answers["score"].(*c.ScoreAnswer).Score != float64(n-1) {
				t.Fatal("wrong score")
			}
		})
	}
}

func TestConfigAndInvalidCalls(t *testing.T) {
	for _, mutate := range []func(*bailian.Config){
		func(x *bailian.Config) { x.APIKey = "" }, func(x *bailian.Config) { x.APIKey = "bad\nkey" },
		func(x *bailian.Config) { x.Model = "" }, func(x *bailian.Config) { x.Model = "\xff" },
		func(x *bailian.Config) { x.Endpoint = "" }, func(x *bailian.Config) { x.Endpoint = "ftp://host" },
		func(x *bailian.Config) { x.Endpoint = "://" }, func(x *bailian.Config) { x.Endpoint = "https:///path" },
		func(x *bailian.Config) { x.Endpoint = "https://user:pass@host/path" },
		func(x *bailian.Config) { x.Endpoint = "https://host/path?key=secret" },
		func(x *bailian.Config) { x.Endpoint = "https://host/path#fragment" },
		func(x *bailian.Config) { x.MaxRequestBytes = -1 }, func(x *bailian.Config) { x.MaxResponseBytes = -1 },
		func(x *bailian.Config) { x.MaxResponseBytes = math.MaxInt64 },
		func(x *bailian.Config) { x.HTTPClient = &http.Client{Timeout: time.Second} },
	} {
		cfg := config("https://fixture.invalid/systemone")
		mutate(&cfg)
		if _, err := bailian.New(cfg); !errors.Is(err, c.ErrInvalidRequest) {
			t.Fatalf("invalid config accepted: %v", err)
		}
	}
	for _, cl := range []*bailian.Client{nil, {}, client(t, config("https://fixture.invalid/systemone"))} {
		var missing context.Context
		out, err := cl.Classify(missing, request())
		checkReport(t, request(), out, err, 0)
		if !errors.Is(err, c.ErrInvalidRequest) {
			t.Fatal(err)
		}
	}
	cl := client(t, config("https://fixture.invalid/systemone"))
	r := request()
	r.State = json.RawMessage(`[]`)
	out, err := cl.Classify(context.Background(), r)
	checkReport(t, r, out, err, 0)
	if !errors.Is(err, c.ErrInvalidRequest) {
		t.Fatal(err)
	}
}

func TestManagedDispatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		limits   c.DispatchLimits
		reject   bool
		attempts int
		want     error
	}{
		{"guard", c.DispatchLimits{}, true, 0, context.Canceled},
		{"negative request", c.DispatchLimits{MaxRequestBytes: -1}, false, 0, c.ErrInvalidRequest},
		{"negative response", c.DispatchLimits{MaxResponseBytes: -1}, false, 0, c.ErrInvalidRequest},
		{"overflow response", c.DispatchLimits{MaxResponseBytes: math.MaxInt64}, false, 0, c.ErrInvalidRequest},
		{"request limit", c.DispatchLimits{MaxRequestBytes: 1}, false, 0, c.ErrInvalidRequest},
		{"response limit", c.DispatchLimits{MaxResponseBytes: 1}, false, 1, c.ErrInvalidResponse},
		{"success", c.DispatchLimits{MaxRequestBytes: 4096, MaxResponseBytes: 4096}, false, 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, fixture) }))
			defer s.Close()
			cl := client(t, config(s.URL))
			endpoint, model := cl.Target()
			if endpoint != s.URL || model != "decision-model-preview" {
				t.Fatal("wrong target")
			}
			guardCalls := 0
			out, err := cl.ClassifyWithDispatch(context.Background(), request(), tc.limits, func(kind c.AttemptKind) error {
				guardCalls++
				if kind != c.PhysicalAttempt {
					t.Error("wrong kind")
				}
				if tc.reject {
					return context.Canceled
				}
				return nil
			})
			checkReport(t, request(), out, err, tc.attempts)
			if !errors.Is(err, tc.want) || calls.Load() != int32(tc.attempts) {
				t.Fatalf("err=%v calls=%d", err, calls.Load())
			}
			wantGuard := tc.attempts
			if tc.reject {
				wantGuard = 1
			}
			if guardCalls != wantGuard {
				t.Fatalf("guard calls=%d want %d", guardCalls, wantGuard)
			}
		})
	}
}

func TestMalformedResponsesAndUsage(t *testing.T) {
	for name, body := range map[string]string{
		"missing binary":          strings.Replace(fixture, `,"noul":0.8`, "", 1),
		"null binary":             strings.Replace(fixture, `"noul":0.8`, `"noul":null`, 1),
		"out of range":            strings.Replace(fixture, `"noul":0.8`, `"noul":1.1`, 1),
		"wrong type":              strings.Replace(fixture, `"type":"noul"`, `"type":"choice"`, 1),
		"missing confidence":      strings.Replace(fixture, `,"confidence":0.6`, "", 1),
		"null probability":        strings.Replace(fixture, `"technical":0.25`, `"technical":null`, 1),
		"incomplete distribution": strings.Replace(fixture, `,"technical":0.25`, "", 1),
		"unknown choice":          strings.Replace(fixture, `"choice":"billing"`, `"choice":"other"`, 1),
		"wrong score":             strings.Replace(fixture, `"score":0.75`, `"score":0.5`, 1),
		"missing score":           strings.Replace(fixture, `,"score":0.75`, "", 1),
		"wrong legend":            strings.Replace(fixture, `"0":"low"`, `"0":"other"`, 1),
		"missing model":           strings.Replace(fixture, `"model":"decision-model-preview",`, "", 1),
		"unknown question":        strings.Replace(fixture, `"binary":`, `"extra":`, 1),
		"bad answers":             strings.Replace(fixture, `"noul":0.8`, `"noul":"private"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer s.Close()
			out, err := client(t, config(s.URL)).Classify(context.Background(), request())
			checkReport(t, request(), out, err, 1)
			if !errors.Is(err, c.ErrInvalidResponse) || out.Attempts[0].Usage == nil || *out.Attempts[0].Usage.InputTokens != 12 {
				t.Fatalf("err=%v usage=%+v", err, out.Attempts[0].Usage)
			}
		})
	}
	for _, tc := range []struct {
		name, usage            string
		input, output, invalid bool
	}{
		{"missing", "", false, false, false}, {"null", `,"usage":null`, false, false, false},
		{"zero", `,"usage":{"input_tokens":0,"output_tokens":0}`, true, true, false},
		{"bad input", `,"usage":{"input_tokens":-1,"output_tokens":0}`, false, true, true},
		{"bad output", `,"usage":{"input_tokens":12,"output_tokens":"bad"}`, true, false, true},
		{"bad usage", `,"usage":[]`, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(fixture, `,"usage":{"input_tokens":12}`, tc.usage, 1)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer s.Close()
			out, err := client(t, config(s.URL)).Classify(context.Background(), request())
			checkReport(t, request(), out, err, 1)
			u := out.Attempts[0].Usage
			if (err != nil) != tc.invalid || (u != nil && u.InputTokens != nil) != tc.input || (u != nil && u.OutputTokens != nil) != tc.output {
				t.Fatalf("err=%v usage=%+v", err, u)
			}
		})
	}
}

func TestHTTPFailures(t *testing.T) {
	for _, status := range []int{302, 307, 400, 401, 403, 422, 429, 500, 529} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/redirect")
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(status)
				fmt.Fprint(w, fixture)
			}))
			defer s.Close()
			out, err := client(t, config(s.URL)).Classify(context.Background(), request())
			checkReport(t, request(), out, err, 1)
			var a *ae.AgentError
			var h *bailian.HTTPError
			if !errors.As(err, &a) || a.Code != "classifier.failed" || !errors.As(err, &h) || h.StatusCode != status || a.Retryable != (status == 429 || status >= 500) || calls.Load() != 1 || *out.Attempts[0].Usage.InputTokens != 12 {
				t.Fatal(err)
			}
			if a.Retryable && a.RetryAfter != 2*time.Second {
				t.Fatal("missing retry hint")
			}
			if h.Error() != fmt.Sprintf("Bailian HTTP status %d", status) {
				t.Fatal(h)
			}
		})
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("private-body") }
func (errorReader) Close() error             { return nil }

func TestTransportAndLateCancellation(t *testing.T) {
	for _, mode := range []string{"network", "read", "deadline", "late cancel", "invalid JSON", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			cfg := config("https://fixture.invalid/systemone")
			cfg.HTTPClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.GetBody != nil {
					t.Error("request replay enabled")
				}
				switch mode {
				case "network":
					return nil, errors.New("fixture-secret private-body")
				case "read":
					return &http.Response{StatusCode: 200, Body: errorReader{}}, nil
				case "deadline":
					return nil, fmt.Errorf("private-body: %w", context.DeadlineExceeded)
				case "late cancel":
					cancel()
				}
				body := fixture
				if mode == "invalid JSON" {
					body = `{"private-body":`
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			if mode == "oversized" {
				cfg.MaxResponseBytes = 1
			}
			out, err := client(t, cfg).Classify(ctx, request())
			checkReport(t, request(), out, err, 1)
			if err == nil || calls != 1 {
				t.Fatal(err)
			}
			for e := err; e != nil; e = errors.Unwrap(e) {
				if strings.Contains(e.Error(), "private-body") || strings.Contains(e.Error(), "fixture-secret") {
					t.Fatal("leaked private error")
				}
			}
			if mode == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if mode == "late cancel" && (!errors.Is(err, context.Canceled) || out.Attempts[0].Usage == nil) {
				t.Fatal("lost late cancel or accounting")
			}
		})
	}
}

func TestCancellation(t *testing.T) {
	started := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	cl := client(t, config(s.URL))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := cl.Classify(ctx, request())
	checkReport(t, request(), out, err, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); out, err = cl.Classify(ctx, request()) }()
	<-started
	cancel()
	<-done
	checkReport(t, request(), out, err, 1)
	var a *ae.AgentError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &a) || a.Code != "classifier.canceled" || a.Retryable {
		t.Fatal(err)
	}
}

func TestConcurrentAndOptionalCriteria(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"criteria"`) {
			t.Error("empty binary criteria emitted")
		}
		fmt.Fprint(w, `{"model":"decision-model-preview","answers":{"binary":{"type":"noul","noul":0}}}`)
	}))
	defer s.Close()
	r := request()
	r.State = json.RawMessage(`"a ticket"`)
	r.Questions = map[string]c.Question{"binary": &c.BinaryQuestion{Instructions: "true?"}}
	cl := client(t, config(s.URL))
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := cl.Classify(context.Background(), r)
			if err != nil || out.Answers["binary"].(*c.BinaryAnswer).Probability != 0 {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestConfiguredCapsCannotBeRaised(t *testing.T) {
	for _, response := range []bool{false, true} {
		calls := 0
		cfg := config("https://fixture.invalid/systemone")
		cfg.HTTPClient = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fixture))}, nil
		})}
		want, attempts := c.ErrInvalidRequest, 0
		if response {
			cfg.MaxResponseBytes = 1
			want, attempts = c.ErrInvalidResponse, 1
		} else {
			cfg.MaxRequestBytes = 1
		}
		cl := client(t, cfg)
		out, err := cl.ClassifyWithDispatch(context.Background(), request(), c.DispatchLimits{MaxRequestBytes: 4096, MaxResponseBytes: 4096}, nil)
		checkReport(t, request(), out, err, attempts)
		if !errors.Is(err, want) || calls != attempts {
			t.Fatal("managed limits raised configured cap")
		}
	}
	// Narrowing one call must not mutate the shared client configuration.
	cl := client(t, config("https://fixture.invalid/systemone"))
	for _, limit := range []int64{1, 4096} {
		stop := errors.New("guard")
		_, err := cl.ClassifyWithDispatch(context.Background(), request(), c.DispatchLimits{MaxRequestBytes: limit}, func(c.AttemptKind) error { return stop })
		if limit == 1 && !errors.Is(err, c.ErrInvalidRequest) || limit == 4096 && err != stop {
			t.Fatal("per-call limit persisted")
		}
	}
}

func TestClientCopyDisablesRedirectsAndCookies(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); fmt.Fprint(w, fixture) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("Cookie") != "" {
			t.Error("caller cookies sent")
		}
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	redirectCalls := 0
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "fixture-cookie"}})
	hc := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { redirectCalls++; return nil }}
	cfg := config(source.URL)
	cfg.HTTPClient = hc
	cl := client(t, cfg)
	out, err := cl.Classify(context.Background(), request())
	checkReport(t, request(), out, err, 1)
	if err == nil || targetCalls.Load() != 0 || redirectCalls != 0 {
		t.Fatal("followed redirect")
	}
	resp, err := hc.Get(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if targetCalls.Load() != 1 || redirectCalls != 1 || len(hc.Jar.Cookies(u)) != 1 {
		t.Fatal("modified caller client")
	}
}

func TestDefaultByteCaps(t *testing.T) {
	cl := client(t, config("https://fixture.invalid/systemone"))
	r := request()
	r.State, _ = json.Marshal(strings.Repeat("x", 1<<20))
	_, err := cl.ClassifyWithDispatch(context.Background(), r, c.DispatchLimits{}, func(c.AttemptKind) error { t.Fatal("oversized request reached guard"); return nil })
	if !errors.Is(err, c.ErrInvalidRequest) {
		t.Fatal(err)
	}
	cfg := config("https://fixture.invalid/systemone")
	cfg.HTTPClient = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", (4<<20)+1)))}, nil
	})}
	out, err := client(t, cfg).Classify(context.Background(), request())
	checkReport(t, request(), out, err, 1)
	if !errors.Is(err, c.ErrInvalidResponse) || out.Attempts[0].Usage != nil {
		t.Fatal("oversized response accepted")
	}
}

func TestDeadlineDuringResponseRead(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	out, err := client(t, config(s.URL)).Classify(ctx, request())
	checkReport(t, request(), out, err, 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestRetryAfterHints(t *testing.T) {
	for _, tc := range []struct {
		header   string
		positive bool
	}{
		{"0", false}, {"-1", false}, {"99999999999999999", false}, {"invalid", false}, {"", false},
		{time.Now().Add(time.Hour).UTC().Format(http.TimeFormat), true},
		{time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), false},
	} {
		t.Run(tc.header, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", tc.header)
				w.WriteHeader(429)
				fmt.Fprint(w, `{"private":"fixture-secret"`)
			}))
			defer s.Close()
			out, err := client(t, config(s.URL)).Classify(context.Background(), request())
			checkReport(t, request(), out, err, 1)
			var a *ae.AgentError
			var h *bailian.HTTPError
			if !errors.As(err, &a) || !a.Retryable || !errors.As(err, &h) || h.StatusCode != 429 || (a.RetryAfter > 0) != tc.positive || strings.Contains(err.Error(), "fixture-secret") {
				t.Fatal(err)
			}
		})
	}
}
