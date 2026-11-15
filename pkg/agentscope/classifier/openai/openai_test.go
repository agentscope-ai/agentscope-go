package openai_test

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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/openai"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/credential"
	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

const success = `{"model":"resolved-model","usage":{"input_tokens":12,"output_tokens":0},"answers":[{"name":"bug","type":"predicate","probability":0.8},{"name":"route","type":"choice","choice":"support","confidence":0.7,"probabilities":[{"value":"other","probability":0.1},{"value":"support","probability":0.9}]},{"name":"severity","type":"score","score":1.1,"confidence":0.6,"probabilities":[{"value":2,"label":"high","probability":0.2},{"value":0,"label":"low","probability":0.1},{"value":1,"label":"medium","probability":0.7}]}]}`

func request() c.Request {
	return c.Request{OperationID: "ticket-1", State: json.RawMessage(`"导出失败"`), Questions: map[string]c.Question{
		"bug":      &c.BinaryQuestion{Instructions: "Is this a bug?", True: "A defect.", False: "A feature request."},
		"route":    &c.ChoiceQuestion{Instructions: "Select department.", Choices: map[string]string{"support": "Technical support.", "other": "Other."}},
		"severity": &c.ScoreQuestion{Instructions: "Rate severity.", Levels: []string{"low", "medium", "high"}},
	}}
}
func config(base string) openai.Config {
	return openai.Config{Credential: &credential.OpenAICredential{Key: "secret-key", URL: base}, Model: "requested-alias"}
}
func client(t *testing.T, cfg openai.Config) *openai.Client {
	t.Helper()
	cl, e := openai.New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	return cl
}
func assertCode(t *testing.T, err error, code string) *ae.AgentError {
	t.Helper()
	var e *ae.AgentError
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error=%v; want %s", err, code)
	}
	return e
}
func assertReport(t *testing.T, r c.Request, out *c.Response, err error, n int) {
	t.Helper()
	if e := c.ValidateReport(r, out); e != nil {
		t.Fatal(e)
	}
	if len(out.Attempts) != n {
		t.Fatalf("attempts=%d want %d", len(out.Attempts), n)
	}
	if n == 1 {
		a := out.Attempts[0]
		if a.Kind != c.PhysicalAttempt || a.RequestedModel != "requested-alias" || a.Duration < 0 || a.Err != err {
			t.Fatalf("invalid attempt: %+v", a)
		}
	}
}

func TestMapping(t *testing.T) {
	for _, state := range []json.RawMessage{json.RawMessage(`"导出失败"`), json.RawMessage(`{ "text": "导出失败" }`)} {
		t.Run(string(state), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/decisions" || r.Header.Get("Authorization") != "Bearer secret-key" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("invalid HTTP request")
				}
				var body struct {
					Model     string
					Input     string
					Questions []struct {
						Type, Name, Instructions string
						Choices                  []struct{ Value, Description string }
						Levels                   []struct{ Label string }
					}
				}
				if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
					t.Error(e)
				}
				if body.Model != "requested-alias" || len(body.Questions) != 3 {
					t.Error("model/questions lost")
				}
				if state[0] == '"' && body.Input != "导出失败" {
					t.Error("JSON string was not decoded")
				}
				if state[0] == '{' && body.Input != `{"text":"导出失败"}` {
					t.Errorf("input=%s", body.Input)
				}
				if len(body.Questions) == 3 {
					b, q, s := body.Questions[0], body.Questions[1], body.Questions[2]
					if b.Name != "bug" || b.Type != "predicate" || b.Instructions != "Is this a bug?\nTrue: A defect.\nFalse: A feature request." {
						t.Errorf("binary=%+v", b)
					}
					if q.Type != "choice" || len(q.Choices) != 2 || q.Choices[0].Value != "other" || q.Choices[1].Description != "Technical support." {
						t.Error("choice mapping")
					}
					if s.Type != "score" || len(s.Levels) != 3 || s.Levels[1].Label != "medium" {
						t.Error("score mapping")
					}
				}
				fmt.Fprint(w, success)
			}))
			defer srv.Close()
			cl := client(t, config(srv.URL+"/v1/"))
			endpoint, model := cl.Target()
			if endpoint != srv.URL+"/v1/decisions" || model != "requested-alias" {
				t.Fatal("target")
			}
			r := request()
			r.State = state
			before, _ := json.Marshal(r.State)
			out, e := cl.Classify(context.Background(), r)
			if e != nil {
				t.Fatal(e)
			}
			assertReport(t, r, out, e, 1)
			if e = c.ValidateResponse(r, out); e != nil {
				t.Fatal(e)
			}
			if out.Model != "resolved-model" || out.Attempts[0].Model != out.Model || *out.Attempts[0].Usage.InputTokens != 12 || *out.Attempts[0].Usage.OutputTokens != 0 {
				t.Fatal("model/usage")
			}
			if out.Answers["route"].(*c.ChoiceAnswer).Confidence != 0.7 || out.Answers["severity"].(*c.ScoreAnswer).Probabilities[2] != 0.2 {
				t.Fatal("values changed")
			}
			after, _ := json.Marshal(r.State)
			if !bytes.Equal(before, after) {
				t.Fatal("input mutated")
			}
		})
	}
}

func TestInvalidResponses(t *testing.T) {
	cases := map[string]string{
		"missing model":              strings.Replace(success, `"model":"resolved-model",`, "", 1),
		"empty model":                strings.Replace(success, `"resolved-model"`, `""`, 1),
		"missing answer":             strings.Replace(success, `{"name":"bug","type":"predicate","probability":0.8},`, "", 1),
		"duplicate answer":           strings.Replace(success, `"name":"route"`, `"name":"bug"`, 1),
		"unknown answer":             strings.Replace(success, `"name":"route"`, `"name":"unknown"`, 1),
		"null name":                  strings.Replace(success, `"name":"route"`, `"name":null`, 1),
		"type mismatch":              strings.Replace(success, `"type":"predicate"`, `"type":"choice"`, 1),
		"missing probability":        strings.Replace(success, `,"probability":0.8`, "", 1),
		"null probability":           strings.Replace(success, `"probability":0.8`, `"probability":null`, 1),
		"out of range":               strings.Replace(success, `"probability":0.8`, `"probability":1.2`, 1),
		"missing confidence":         strings.Replace(success, `,"confidence":0.7`, "", 1),
		"null confidence":            strings.Replace(success, `"confidence":0.7`, `"confidence":null`, 1),
		"boolean choice":             strings.Replace(success, `"choice":"support"`, `"choice":true`, 1),
		"boolean candidate":          strings.Replace(success, `"value":"other"`, `"value":false`, 1),
		"duplicate candidate":        strings.Replace(success, `"value":"other"`, `"value":"support"`, 1),
		"unknown candidate":          strings.Replace(success, `"value":"other"`, `"value":"unknown"`, 1),
		"missing candidate":          strings.Replace(success, `{"value":"other","probability":0.1},`, "", 1),
		"null candidate probability": strings.Replace(success, `"probability":0.9`, `"probability":null`, 1),
		"sum mismatch":               strings.Replace(success, `"probability":0.9`, `"probability":0.8`, 1),
		"not maximum":                strings.Replace(success, `"choice":"support"`, `"choice":"other"`, 1),
		"missing score":              strings.Replace(success, `,"score":1.1`, "", 1),
		"score mismatch":             strings.Replace(success, `"score":1.1`, `"score":1.2`, 1),
		"missing level":              strings.Replace(success, `{"value":2,"label":"high","probability":0.2},`, "", 1),
		"duplicate level":            strings.Replace(success, `"value":2`, `"value":1`, 1),
		"negative level":             strings.Replace(success, `"value":2`, `"value":-1`, 1),
		"unknown level":              strings.Replace(success, `"value":2`, `"value":3`, 1),
		"fractional level":           strings.Replace(success, `"value":2`, `"value":1.5`, 1),
		"missing index":              strings.Replace(success, `"value":0,`, "", 1),
		"null index":                 strings.Replace(success, `"value":0`, `"value":null`, 1),
		"wrong label":                strings.Replace(success, `"label":"high"`, `"label":"wrong"`, 1),
		"null level probability":     strings.Replace(success, `"probability":0.2`, `"probability":null`, 1),
		"wrong usage":                strings.Replace(success, `"input_tokens":12`, `"input_tokens":-1`, 1),
		"wrong output usage":         strings.Replace(success, `"output_tokens":0`, `"output_tokens":"bad"`, 1),
		"null answers":               strings.Replace(success, `"answers":[`, `"answers":null,"ignored":[`, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
			defer srv.Close()
			r := request()
			out, e := client(t, config(srv.URL)).Classify(context.Background(), r)
			assertCode(t, e, c.ErrInvalidResponse.Code)
			assertReport(t, r, out, e, 1)
			if len(out.Answers) != 0 {
				t.Fatal("invalid response exposes answers")
			}
			u := out.Attempts[0].Usage
			if u == nil {
				t.Fatal("lost usage")
			}
			if name != "wrong usage" && (u.InputTokens == nil || *u.InputTokens != 12) {
				t.Fatal("lost input usage")
			}
			if name != "wrong output usage" && (u.OutputTokens == nil || *u.OutputTokens != 0) {
				t.Fatal("lost output usage")
			}
		})
	}
}

func TestRefusalAndHTTPFailures(t *testing.T) {
	for _, status := range []int{200, 400, 401, 429, 500, 529, 302} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "2")
				w.Header().Set("Location", "/elsewhere")
				w.WriteHeader(status)
				fmt.Fprint(w, strings.Replace(success, `{"name":"bug","type":"predicate","probability":0.8}`, `{"name":"bug","type":"refusal"}`, 1))
			}))
			defer srv.Close()
			r := request()
			out, e := client(t, config(srv.URL)).Classify(context.Background(), r)
			a := assertCode(t, e, "classifier.failed")
			assertReport(t, r, out, e, 1)
			if calls.Load() != 1 || len(out.Answers) != 0 || out.Attempts[0].Usage == nil {
				t.Fatal("retry/answers/accounting")
			}
			if a.Retryable != (status == 429 || status >= 500) {
				t.Fatal("retry hint")
			}
			if a.Retryable && a.RetryAfter != 2*time.Second {
				t.Fatal("Retry-After")
			}
			if status != 200 {
				var h *openai.HTTPError
				if !errors.As(e, &h) || h.StatusCode != status {
					t.Fatal("HTTP status missing")
				}
			}
		})
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestConfiguration(t *testing.T) {
	good := config("https://api.openai.com/v1")
	for name, mutate := range map[string]func(*openai.Config){"nil credential": func(c *openai.Config) { c.Credential = nil }, "empty key": func(c *openai.Config) { c.Credential.Key = "" }, "bad key": func(c *openai.Config) { c.Credential.Key = "key\nsecret" }, "empty model": func(c *openai.Config) { c.Model = "" }, "bad model": func(c *openai.Config) { c.Model = string([]byte{0xff}) }, "query": func(c *openai.Config) { c.Credential.URL = "https://example.com/v1?key=secret" }, "fragment": func(c *openai.Config) { c.Credential.URL = "https://example.com/v1#x" }, "userinfo": func(c *openai.Config) { c.Credential.URL = "https://user:secret@example.com/v1" }, "scheme": func(c *openai.Config) { c.Credential.URL = "file:///tmp/data" }, "negative request": func(c *openai.Config) { c.MaxRequestBytes = -1 }, "negative response": func(c *openai.Config) { c.MaxResponseBytes = -1 }, "overflow": func(c *openai.Config) { c.MaxResponseBytes = math.MaxInt64 }, "timeout": func(c *openai.Config) { c.HTTPClient = &http.Client{Timeout: time.Second} }} {
		t.Run(name, func(t *testing.T) {
			cfg := good
			cred := *good.Credential
			cfg.Credential = &cred
			mutate(&cfg)
			_, e := openai.New(cfg)
			assertCode(t, e, c.ErrInvalidRequest.Code)
			if strings.Contains(e.Error(), "secret") {
				t.Fatal("secret leaked")
			}
		})
	}
	cl := client(t, openai.Config{Credential: &credential.OpenAICredential{Key: "key"}, Model: "model"})
	endpoint, _ := cl.Target()
	if endpoint != openai.DefaultEndpoint {
		t.Fatal(endpoint)
	}
	// Constructor snapshots both credentials and client settings.
	cfg := config("https://example.com/v1")
	cfg.HTTPClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer secret-key" || r.GetBody != nil || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("Cookie") != "" {
			t.Error("request replay/auth/cookies")
		}
		return response(success), nil
	})}
	jar, _ := cookiejar.New(nil)
	cookieURL, _ := url.Parse(cfg.Credential.BaseURL())
	jar.SetCookies(cookieURL, []*http.Cookie{{Name: "session", Value: "private-cookie"}})
	cfg.HTTPClient.Jar = jar
	cl = client(t, cfg)
	cfg.Credential.Key = "changed"
	cfg.Credential.URL = "https://changed.example"
	cfg.HTTPClient.Timeout = time.Nanosecond
	out, e := cl.Classify(context.Background(), request())
	if e != nil || len(out.Answers) != 3 {
		t.Fatal(e)
	}
}

func TestPreDispatch(t *testing.T) {
	var calls atomic.Int32
	cfg := config("https://example.com/v1")
	cfg.HTTPClient = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return response(success), nil })}
	cl := client(t, cfg)
	for name, run := range map[string]func() (*c.Response, error){
		"nil client": func() (*c.Response, error) {
			var cl *openai.Client
			return cl.Classify(context.Background(), request())
		},
		"zero client": func() (*c.Response, error) { return new(openai.Client).Classify(context.Background(), request()) },
		"nil context": func() (*c.Response, error) {
			return cl.Classify(nil, request()) //nolint:staticcheck // Exercise the documented nil-context rejection.
		},
		"invalid request": func() (*c.Response, error) {
			r := request()
			r.OperationID = ""
			return cl.Classify(context.Background(), r)
		},
		"choice limit": func() (*c.Response, error) {
			r := request()
			r.Questions["route"] = &c.ChoiceQuestion{Instructions: "choose", Choices: map[string]string{"a": ""}}
			return cl.Classify(context.Background(), r)
		},
		"request cap": func() (*c.Response, error) {
			return cl.ClassifyWithDispatch(context.Background(), request(), c.DispatchLimits{MaxRequestBytes: 1}, nil)
		},
		"bad limits": func() (*c.Response, error) {
			return cl.ClassifyWithDispatch(context.Background(), request(), c.DispatchLimits{MaxResponseBytes: math.MaxInt64}, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, e := run()
			assertCode(t, e, c.ErrInvalidRequest.Code)
			if out == nil || len(out.Attempts) != 0 {
				t.Fatal("pre-dispatch accounting")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, e := cl.Classify(ctx, request())
	assertCode(t, e, "classifier.canceled")
	if !errors.Is(e, context.Canceled) || len(out.Attempts) != 0 {
		t.Fatal("cancel")
	}
	denied := errors.New("host denied")
	out, e = cl.ClassifyWithDispatch(context.Background(), request(), c.DispatchLimits{}, func(k c.AttemptKind) error {
		if k != c.PhysicalAttempt {
			t.Fatal(k)
		}
		return denied
	})
	if e != denied || len(out.Attempts) != 0 || calls.Load() != 0 {
		t.Fatal("guard bypass")
	}
}

func TestTransportAndCancellation(t *testing.T) {
	for _, cause := range []error{errors.New("secret provider body"), fmt.Errorf("secret: %w", context.Canceled), fmt.Errorf("secret: %w", context.DeadlineExceeded)} {
		t.Run(fmt.Sprint(cause), func(t *testing.T) {
			cfg := config("https://example.com/v1")
			cfg.HTTPClient = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return nil, cause })}
			out, e := client(t, cfg).Classify(context.Background(), request())
			assertReport(t, request(), out, e, 1)
			if strings.Contains(fmt.Sprintf("%+v", e), "secret") {
				t.Fatal("leaked cause")
			}
			if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
				assertCode(t, e, "classifier.canceled")
				if !errors.Is(e, cause) && !(errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded)) {
					t.Fatal("lost cancellation")
				}
			} else {
				assertCode(t, e, "classifier.failed")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := config("https://example.com/v1")
	cfg.HTTPClient = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { cancel(); return response(success), nil })}
	out, e := client(t, cfg).Classify(ctx, request())
	assertCode(t, e, "classifier.canceled")
	if !errors.Is(e, context.Canceled) || out.Attempts[0].Usage == nil || len(out.Answers) != 0 {
		t.Fatal("late cancel")
	}
}

func TestBodiesAndUsage(t *testing.T) {
	for name, body := range map[string]string{"malformed": "{secret", "null": "null", "array": "[]", "trailing": success + " {}", "invalid UTF8": success + string([]byte{0xff})} {
		t.Run(name, func(t *testing.T) {
			cfg := config("https://example.com/v1")
			cfg.HTTPClient = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return response(body), nil })}
			out, e := client(t, cfg).Classify(context.Background(), request())
			assertCode(t, e, c.ErrInvalidResponse.Code)
			if out.Attempts[0].Usage != nil || strings.Contains(e.Error(), "secret") {
				t.Fatal("untrusted usage/body")
			}
		})
	}
	for name, usage := range map[string]string{"missing": "", "null": `,"usage":null`, "empty": `,"usage":{}`, "unknown": `,"usage":{"input_tokens":null}`, "invalid": `,"usage":[]`} {
		t.Run("usage/"+name, func(t *testing.T) {
			body := strings.Replace(success, `,"usage":{"input_tokens":12,"output_tokens":0}`, usage, 1)
			cfg := config("https://example.com/v1")
			cfg.HTTPClient = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return response(body), nil })}
			out, e := client(t, cfg).Classify(context.Background(), request())
			if name == "invalid" {
				assertCode(t, e, c.ErrInvalidResponse.Code)
			} else if e != nil {
				t.Fatal(e)
			}
			u := out.Attempts[0].Usage
			if u != nil && (u.InputTokens != nil || u.OutputTokens != nil) {
				t.Fatal("invented tokens")
			}
		})
	}
	cfg := config("https://example.com/v1")
	cfg.HTTPClient = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		res := response(success)
		res.StatusCode = 429
		return res, nil
	})}
	out, e := client(t, cfg).ClassifyWithDispatch(context.Background(), request(), c.DispatchLimits{MaxResponseBytes: 5}, nil)
	a := assertCode(t, e, c.ErrInvalidResponse.Code)
	if a.Retryable || out.Attempts[0].Usage != nil {
		t.Fatal("oversized response trusted")
	}
}

func TestDispatchAndConcurrentCalls(t *testing.T) {
	var calls atomic.Int32
	cfg := config("https://example.com/v1")
	cfg.HTTPClient = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return response(success), nil })}
	cl := client(t, cfg)
	for i := 0; i < 10; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			var guards int
			out, e := cl.ClassifyWithDispatch(context.Background(), request(), c.DispatchLimits{MaxRequestBytes: 1 << 22, MaxResponseBytes: 1 << 22}, func(c.AttemptKind) error { guards++; return nil })
			if e != nil || guards != 1 || len(out.Answers) != 3 {
				t.Fatal("dispatch")
			}
		})
	}
}

type readFailure struct {
	closed bool
	cause  error
}

func (r *readFailure) Read([]byte) (int, error) { return 0, r.cause }
func (r *readFailure) Close() error             { r.closed = true; return nil }

func TestResponseReadFailure(t *testing.T) {
	for _, cause := range []error{errors.New("secret-read-error"), context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			body := &readFailure{cause: cause}
			cfg := config("https://example.com/prefix/v1")
			cfg.HTTPClient = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"2"}}, Body: body}, nil
			})}
			out, e := client(t, cfg).Classify(context.Background(), request())
			code := "classifier.failed"
			if errors.Is(cause, context.DeadlineExceeded) {
				code = "classifier.canceled"
			}
			a := assertCode(t, e, code)
			if !body.closed || out.Attempts[0].Usage != nil || a.Retryable || strings.Contains(e.Error(), "secret") {
				t.Fatal("read failure handling")
			}
		})
	}
}

func TestRetryAfterAndMalformedHTTPError(t *testing.T) {
	futureHint := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	for _, hint := range []string{"", "garbage", "-1", "9223372036854775807", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), futureHint} {
		t.Run(hint, func(t *testing.T) {
			cfg := config("https://example.com/v1")
			cfg.HTTPClient = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				r := response("{secret-body")
				r.StatusCode = 429
				r.Header.Set("Retry-After", hint)
				return r, nil
			})}
			out, e := client(t, cfg).Classify(context.Background(), request())
			a := assertCode(t, e, "classifier.failed")
			var h *openai.HTTPError
			if !errors.As(e, &h) || h.Error() != "OpenAI HTTP status 429" || !a.Retryable || out.Attempts[0].Usage != nil {
				t.Fatal("HTTP error")
			}
			if strings.Contains(e.Error(), "secret") {
				t.Fatal("body leaked")
			}
			if hint == futureHint && a.RetryAfter <= 0 {
				t.Fatal("missing date delay")
			}
		})
	}
}

func TestInFlightCancellation(t *testing.T) {
	started := make(chan struct{})
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-released:
		}
		fmt.Fprint(w, success)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	cl := client(t, config(srv.URL))
	var out *c.Response
	var err error
	go func() { out, err = cl.Classify(ctx, request()); close(done) }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(released)
		t.Fatal("cancellation did not stop call")
	}
	close(released)
	assertCode(t, err, "classifier.canceled")
	assertReport(t, request(), out, err, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("lost cancellation")
	}
}

func TestEncodedLimitsAndProviderBounds(t *testing.T) {
	var sent int
	cfg := config("https://example.com/v1")
	cfg.HTTPClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		sent = len(b)
		return response(success), nil
	})}
	cl := client(t, cfg)
	if _, e := cl.Classify(context.Background(), request()); e != nil {
		t.Fatal(e)
	}
	cfg.MaxRequestBytes = int64(sent)
	cfg.MaxResponseBytes = int64(len(success))
	cl = client(t, cfg)
	if _, e := cl.Classify(context.Background(), request()); e != nil {
		t.Fatal("exact limit:", e)
	}
	out, e := cl.ClassifyWithDispatch(context.Background(), request(), c.DispatchLimits{MaxRequestBytes: int64(sent - 1)}, nil)
	assertCode(t, e, c.ErrInvalidRequest.Code)
	if len(out.Attempts) != 0 {
		t.Fatal("local limit dispatched")
	}
	out, e = cl.ClassifyWithDispatch(context.Background(), request(), c.DispatchLimits{MaxResponseBytes: int64(len(success) - 1)}, nil)
	assertCode(t, e, c.ErrInvalidResponse.Code)
	if len(out.Attempts) != 1 {
		t.Fatal("response limit attempt")
	}
	for _, limit := range []c.DispatchLimits{{MaxRequestBytes: -1}, {MaxResponseBytes: -1}} {
		_, e := cl.ClassifyWithDispatch(context.Background(), request(), limit, nil)
		assertCode(t, e, c.ErrInvalidRequest.Code)
	}
	for name, mutate := range map[string]func(c.Request){"many choices": func(r c.Request) {
		q := r.Questions["route"].(*c.ChoiceQuestion)
		for i := 0; i < 254; i++ {
			q.Choices[fmt.Sprint(i)] = ""
		}
	}, "binary UTF8": func(r c.Request) { r.Questions["bug"].(*c.BinaryQuestion).True = string([]byte{0xff}) }, "choice UTF8": func(r c.Request) { r.Questions["route"].(*c.ChoiceQuestion).Choices["support"] = string([]byte{0xff}) }} {
		t.Run(name, func(t *testing.T) {
			r := request()
			mutate(r)
			out, e := cl.Classify(context.Background(), r)
			assertCode(t, e, c.ErrInvalidRequest.Code)
			if len(out.Attempts) != 0 {
				t.Fatal("invalid provider input dispatched")
			}
		})
	}
	var nilClient *openai.Client
	if endpoint, model := nilClient.Target(); endpoint != "" || model != "" {
		t.Fatal("nil target")
	}
}
