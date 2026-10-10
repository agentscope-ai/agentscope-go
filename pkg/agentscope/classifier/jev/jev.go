// Package jev implements the experimental TypeSafe System One HTTP classifier.
// Hosts must authorize and project all state and question data before Classify.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

// DefaultEndpoint is the full TypeSafe evaluation URL.
const DefaultEndpoint = "https://api.typesafe.ai/v1/systemone"

// Config configures a single-attempt client. Model is required; no moving alias
// is selected implicitly. Endpoint is a full URL, not a base URL.
type Config struct {
	APIKey   string
	Model    string
	Endpoint string
	// HTTPClient is copied. Its Timeout must be zero: deadlines belong to ctx.
	// Redirects and cookies are disabled on the copy. Transport is shared and must
	// honor context, make no retries, and be safe for concurrent use. The caller
	// owns it and must not mutate it while calls are in progress.
	HTTPClient *http.Client
	// Byte limits include the complete encoded request / decoded HTTP body.
	// Zero selects 1 MiB / 4 MiB respectively. Negative limits are invalid.
	MaxRequestBytes  int64
	MaxResponseBytes int64
}

// Client is safe for concurrent calls when its transport is. Construct with New.
type Client struct {
	key, model, endpoint        string
	http                        http.Client
	requestLimit, responseLimit int64
}

var _ c.Classifier = (*Client)(nil)

// New validates configuration without making a request. It never reads keys
// from the environment. The default HTTP transport has no whole-call timeout.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" || strings.TrimSpace(cfg.Model) == "" || !utf8.ValidString(cfg.Model) {
		return nil, invalidRequest("API key and model are required")
	}
	for _, v := range []byte(cfg.APIKey) {
		if v < 0x21 || v > 0x7e {
			return nil, invalidRequest("API key must be a visible ASCII token")
		}
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || u.Opaque != "" {
		return nil, invalidRequest("endpoint must be an HTTP(S) URL without credentials, query or fragment")
	}
	if cfg.MaxRequestBytes < 0 || cfg.MaxResponseBytes < 0 || cfg.MaxResponseBytes == math.MaxInt64 {
		return nil, invalidRequest("invalid byte limits")
	}
	if cfg.MaxRequestBytes == 0 {
		cfg.MaxRequestBytes = 1 << 20
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = 4 << 20
	}
	hc := http.Client{}
	if cfg.HTTPClient != nil {
		hc = *cfg.HTTPClient
	}
	if hc.Timeout != 0 {
		return nil, invalidRequest("HTTPClient.Timeout must be zero; use a context deadline")
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	hc.Jar = nil
	return &Client{key: cfg.APIKey, model: cfg.Model, endpoint: cfg.Endpoint, http: hc, requestLimit: cfg.MaxRequestBytes, responseLimit: cfg.MaxResponseBytes}, nil
}

// HTTPError exposes only the status, never the provider's raw error body.
// It is the cause of a classifier.failed AgentError. Retryable and RetryAfter
// are hints for a caller's policy; this client does not perform retries.
type HTTPError struct{ StatusCode int }

func (e *HTTPError) Error() string { return fmt.Sprintf("Jev HTTP status %d", e.StatusCode) }
func invalidRequest(s string) error {
	return ae.Newf(ae.CategoryConfig, c.ErrInvalidRequest.Code, "Jev: %s", s)
}
func invalidResponse(s string) error {
	return ae.Newf(ae.CategoryModel, c.ErrInvalidResponse.Code, "Jev: %s", s)
}
func canceled(err error, phase string) error {
	return ae.Wrap(err, ae.CategoryModel, "classifier.canceled", "Jev classification canceled "+phase)
}
func failed(s string) *ae.AgentError {
	return ae.Newf(ae.CategoryModel, "classifier.failed", "Jev: %s", s)
}

// Classify sends at most one HTTP request, returning accounting even on error.
// The dispatch boundary is entry to http.Client.Do, not confirmed server receipt.
// DNS/connection failures therefore retain an attempt with unknown usage.
// No request/response bodies, credentials or raw transport errors are logged.
func (cl *Client) Classify(ctx context.Context, r c.Request) (out *c.Response, err error) {
	out = &c.Response{OperationID: r.OperationID}
	if cl == nil || cl.endpoint == "" {
		return out, invalidRequest("client must be constructed with New")
	}
	if ctx == nil {
		return out, invalidRequest("context is required")
	}
	if e := ctx.Err(); e != nil {
		return out, canceled(e, "before dispatch")
	}
	if e := c.ValidateRequest(r); e != nil {
		return out, e
	}
	payload, e := cl.encode(r)
	if e != nil {
		return out, e
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, cl.endpoint, bytes.NewReader(payload))
	if e != nil {
		return out, invalidRequest("cannot construct HTTP request")
	}
	req.Header.Set("Authorization", "Bearer "+cl.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// A POST without GetBody or an idempotency header cannot be replayed by the
	// standard Transport when a reused connection fails.
	req.GetBody = nil
	if e = ctx.Err(); e != nil {
		return out, canceled(e, "before dispatch")
	}
	start := time.Now()
	out.Attempts = []c.Attempt{{Kind: c.PhysicalAttempt, RequestedModel: cl.model}}
	defer func() {
		if e := ctx.Err(); e != nil {
			err = canceled(e, "after dispatch")
		}
		out.Attempts[0].Duration = time.Since(start)
		out.Attempts[0].Err = err
	}()
	resp, e := cl.http.Do(req)
	if e != nil {
		return out, transportError(e)
	}
	defer resp.Body.Close()
	body, e := io.ReadAll(io.LimitReader(resp.Body, cl.responseLimit+1))
	if e != nil {
		return out, transportError(e)
	}
	if int64(len(body)) > cl.responseLimit {
		return out, invalidResponse("response exceeds byte limit")
	}
	parseErr := decode(body, r, out)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h := &HTTPError{StatusCode: resp.StatusCode}
		f := failed(h.Error())
		f.Cause = h
		f.Retryable = resp.StatusCode == 429 || resp.StatusCode >= 500
		if f.Retryable {
			f.RetryAfter = retryAfter(resp.Header.Get("Retry-After"))
		}
		return out, f
	}
	if parseErr != nil {
		return out, parseErr
	}
	return out, c.ValidateResponse(r, out)
}
func transportError(err error) error {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, cause) {
			return canceled(cause, "after dispatch")
		}
	}
	// Do not retain URL errors or arbitrary transport causes: their strings can
	// contain credentials, task data or an untrusted endpoint response.
	return failed("HTTP transport or response read failed")
}
func retryAfter(s string) time.Duration {
	if n, e := strconv.ParseInt(strings.TrimSpace(s), 10, 64); e == nil {
		if n > 0 && n <= math.MaxInt64/int64(time.Second) {
			return time.Duration(n) * time.Second
		}
		return 0
	}
	if at, e := http.ParseTime(s); e == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return 0
}

type question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

func (cl *Client) encode(r c.Request) ([]byte, error) {
	qs := make(map[string]question, len(r.Questions))
	for id, q := range r.Questions {
		switch q := q.(type) {
		case *c.BinaryQuestion:
			criteria := map[string]string{}
			if q.True != "" {
				criteria["true"] = q.True
			}
			if q.False != "" {
				criteria["false"] = q.False
			}
			w := question{Type: "noul", Instructions: q.Instructions}
			if len(criteria) > 0 {
				w.Criteria = criteria
			}
			qs[id] = w
		case *c.ChoiceQuestion:
			if len(q.Choices) > 255 {
				return nil, invalidRequest("Choice supports at most 255 candidates")
			}
			qs[id] = question{Type: "choice", Instructions: q.Instructions, Criteria: q.Choices}
		case *c.ScoreQuestion:
			if len(q.Levels) < 2 || len(q.Levels) > 10 {
				return nil, invalidRequest("Score requires 2 to 10 levels")
			}
			qs[id] = question{Type: "score", Instructions: q.Instructions, Criteria: q.Levels}
		}
	}
	body, e := json.Marshal(struct {
		Model     string              `json:"model"`
		State     json.RawMessage     `json:"state"`
		Questions map[string]question `json:"questions"`
	}{cl.model, r.State, qs})
	if e != nil {
		return nil, invalidRequest("cannot encode request")
	}
	if int64(len(body)) > cl.requestLimit {
		return nil, invalidRequest("request exceeds byte limit")
	}
	return body, nil
}

// Decode accounting independently of answers so a malformed answer does not
// erase valid provider usage. Invalid token fields remain unknown, not zero.
func decode(body []byte, r c.Request, out *c.Response) error {
	var fields map[string]json.RawMessage
	if !utf8.Valid(body) || json.Unmarshal(body, &fields) != nil || fields == nil {
		return invalidResponse("malformed JSON response")
	}
	var bad bool
	if e := json.Unmarshal(fields["model"], &out.Model); e != nil {
		bad = true
	}
	out.Attempts[0].Model = out.Model
	if raw, ok := fields["usage"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var usage map[string]json.RawMessage
		if json.Unmarshal(raw, &usage) != nil || usage == nil {
			bad = true
		} else {
			u := &c.Usage{}
			out.Attempts[0].Usage = u
			for name, dst := range map[string]**int64{"input_tokens": &u.InputTokens, "output_tokens": &u.OutputTokens} {
				if v, ok := usage[name]; ok {
					var n *int64
					if json.Unmarshal(v, &n) != nil || (n != nil && *n < 0) {
						bad = true
					} else {
						*dst = n
					}
				}
			}
		}
	}
	var answers map[string]json.RawMessage
	if json.Unmarshal(fields["answers"], &answers) != nil || len(answers) != len(r.Questions) {
		return invalidResponse("invalid answer map")
	}
	out.Answers = make(map[string]c.Answer, len(answers))
	for id, raw := range answers {
		q, ok := r.Questions[id]
		if !ok {
			return invalidResponse("unexpected answer ID")
		}
		a, e := decodeAnswer(raw, q)
		if e != nil {
			return e
		}
		out.Answers[id] = a
	}
	if bad {
		return invalidResponse("invalid model or usage")
	}
	return nil
}
func decodeAnswer(raw json.RawMessage, q c.Question) (c.Answer, error) {
	var a struct {
		Type          string              `json:"type"`
		Noul          *float64            `json:"noul"`
		Choice        string              `json:"choice"`
		Confidence    *float64            `json:"confidence"`
		Score         *float64            `json:"score"`
		Probabilities map[string]*float64 `json:"probabilities"`
		Legend        map[string]string   `json:"legend"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return nil, invalidResponse("malformed answer")
	}
	switch q := q.(type) {
	case *c.BinaryQuestion:
		if a.Type == "noul" && a.Noul != nil {
			return &c.BinaryAnswer{Probability: *a.Noul}, nil
		}
	case *c.ChoiceQuestion:
		if a.Type == "choice" && a.Confidence != nil {
			ps := make(map[string]float64, len(a.Probabilities))
			for k, p := range a.Probabilities {
				if p == nil {
					return nil, invalidResponse("null candidate probability")
				}
				ps[k] = *p
			}
			return &c.ChoiceAnswer{Choice: a.Choice, Confidence: *a.Confidence, Probabilities: ps}, nil
		}
	case *c.ScoreQuestion:
		if a.Type == "score" && a.Score != nil && a.Confidence != nil && len(a.Legend) == len(q.Levels) && len(a.Probabilities) == len(q.Levels) {
			ps := make([]float64, len(q.Levels))
			for i, level := range q.Levels {
				k := strconv.Itoa(i)
				p := a.Probabilities[k]
				if p == nil || a.Legend[k] != level {
					return nil, invalidResponse("score levels do not match rubric")
				}
				ps[i] = *p
			}
			return &c.ScoreAnswer{Score: *a.Score, Confidence: *a.Confidence, Probabilities: ps}, nil
		}
	}
	return nil, invalidResponse("missing or mismatched answer fields")
}
