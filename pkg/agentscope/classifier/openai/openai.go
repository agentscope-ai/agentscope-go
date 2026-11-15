// Package openai implements the experimental OpenAI Decisions HTTP classifier.
// Hosts must authorize and project all state and question data before Classify.
package openai

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
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/credential"
	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

// DefaultEndpoint is the full OpenAI Decisions URL.
const DefaultEndpoint = "https://api.openai.com/v1/decisions"

// Config configures a single-attempt client. Model is required; no moving alias
// is selected implicitly. Credential.BaseURL is an API base ending in /v1
// (or a custom prefix); New appends /decisions and snapshots the credential.
type Config struct {
	Credential *credential.OpenAICredential
	Model      string
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

var _ c.DispatchClassifier = (*Client)(nil)

// New validates configuration without making a request. It never reads keys
// from the environment. The default HTTP transport has no whole-call timeout.
func New(cfg Config) (*Client, error) {
	if cfg.Credential == nil {
		return nil, invalidRequest("OpenAI credential is required")
	}
	key := cfg.Credential.APIKey()
	endpoint := strings.TrimRight(cfg.Credential.BaseURL(), "/") + "/decisions"
	if strings.TrimSpace(key) == "" || strings.TrimSpace(cfg.Model) == "" || !utf8.ValidString(cfg.Model) {
		return nil, invalidRequest("API key and model are required")
	}
	for _, v := range []byte(key) {
		if v < 0x21 || v > 0x7e {
			return nil, invalidRequest("API key must be a visible ASCII token")
		}
	}
	u, err := url.Parse(endpoint)
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
	return &Client{key: key, model: cfg.Model, endpoint: endpoint, http: hc, requestLimit: cfg.MaxRequestBytes, responseLimit: cfg.MaxResponseBytes}, nil
}

// HTTPError exposes only the status, never the provider's raw error body.
// It is the cause of a classifier.failed AgentError. Retryable and RetryAfter
// are hints for a caller's policy; this client does not perform retries.
type HTTPError struct{ StatusCode int }

func (e *HTTPError) Error() string { return fmt.Sprintf("OpenAI HTTP status %d", e.StatusCode) }
func invalidRequest(s string) error {
	return ae.Newf(ae.CategoryConfig, c.ErrInvalidRequest.Code, "OpenAI: %s", s)
}
func invalidResponse(s string) error {
	return ae.Newf(ae.CategoryModel, c.ErrInvalidResponse.Code, "OpenAI: %s", s)
}
func canceled(err error, phase string) error {
	return ae.Wrap(err, ae.CategoryModel, "classifier.canceled", "OpenAI classification canceled "+phase)
}
func failed(s string) *ae.AgentError {
	return ae.Newf(ae.CategoryModel, "classifier.failed", "OpenAI: %s", s)
}

// Classify sends at most one HTTP request, returning accounting even on error.
// The dispatch boundary is entry to http.Client.Do, not confirmed server receipt.
// DNS/connection failures therefore retain an attempt with unknown usage.
// No request/response bodies, credentials or raw transport errors are logged.
func (cl *Client) Classify(ctx context.Context, r c.Request) (out *c.Response, err error) {
	return cl.ClassifyWithDispatch(ctx, r, c.DispatchLimits{}, nil)
}

// Target returns the actual endpoint and configured requested version.
func (cl *Client) Target() (endpoint, model string) {
	if cl == nil {
		return "", ""
	}
	return cl.endpoint, cl.model
}

// ClassifyWithDispatch guards the HTTP dispatch after request construction.
// A guard failure preserves its error identity and records no attempt.
func (cl *Client) ClassifyWithDispatch(ctx context.Context, r c.Request, limits c.DispatchLimits, begin func(c.AttemptKind) error) (out *c.Response, err error) {
	out = &c.Response{OperationID: r.OperationID}
	if cl == nil || cl.endpoint == "" {
		return out, invalidRequest("client must be constructed with New")
	}
	if ctx == nil {
		return out, invalidRequest("context is required")
	}
	if limits.MaxRequestBytes < 0 || limits.MaxResponseBytes < 0 || limits.MaxResponseBytes == math.MaxInt64 {
		return out, invalidRequest("invalid managed byte limits")
	}
	copyClient := *cl
	if limits.MaxRequestBytes > 0 && limits.MaxRequestBytes < copyClient.requestLimit {
		copyClient.requestLimit = limits.MaxRequestBytes
	}
	if limits.MaxResponseBytes > 0 && limits.MaxResponseBytes < copyClient.responseLimit {
		copyClient.responseLimit = limits.MaxResponseBytes
	}
	cl = &copyClient
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
	if begin != nil {
		if e := begin(c.PhysicalAttempt); e != nil {
			return out, e
		}
	}
	out.Attempts = []c.Attempt{{Kind: c.PhysicalAttempt, RequestedModel: cl.model}}
	defer func() {
		if e := ctx.Err(); e != nil {
			err = canceled(e, "after dispatch")
		}
		if err != nil {
			out.Answers = nil
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
		f := failed(fmt.Sprintf("HTTP status %d", resp.StatusCode))
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
	Type         string   `json:"type"`
	Name         string   `json:"name"`
	Instructions string   `json:"instructions"`
	Choices      []choice `json:"choices,omitempty"`
	Levels       []level  `json:"levels,omitempty"`
}
type choice struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}
type level struct {
	Label string `json:"label"`
}

func (cl *Client) encode(r c.Request) ([]byte, error) {
	// Stable ordering makes the wire request independent of Go map iteration.
	ids := make([]string, 0, len(r.Questions))
	for id := range r.Questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	qs := make([]question, 0, len(ids))
	for _, id := range ids {
		w := question{Name: id}
		switch q := r.Questions[id].(type) {
		case *c.BinaryQuestion:
			if !utf8.ValidString(q.True) || !utf8.ValidString(q.False) {
				return nil, invalidRequest("binary descriptions must be valid UTF-8")
			}
			w.Type = "predicate"
			w.Instructions = q.Instructions
			if q.True != "" {
				w.Instructions += "\nTrue: " + q.True
			}
			if q.False != "" {
				w.Instructions += "\nFalse: " + q.False
			}
		case *c.ChoiceQuestion:
			if len(q.Choices) < 2 || len(q.Choices) > 255 {
				return nil, invalidRequest("Choice requires 2 to 255 candidates")
			}
			w.Type = "choice"
			w.Instructions = q.Instructions
			keys := make([]string, 0, len(q.Choices))
			for key := range q.Choices {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if !utf8.ValidString(q.Choices[key]) {
					return nil, invalidRequest("choice descriptions must be valid UTF-8")
				}
				w.Choices = append(w.Choices, choice{Value: key, Description: q.Choices[key]})
			}
		case *c.ScoreQuestion:
			w.Type = "score"
			w.Instructions = q.Instructions
			for _, label := range q.Levels {
				w.Levels = append(w.Levels, level{Label: label})
			}
		}
		qs = append(qs, w)
	}
	state := bytes.TrimSpace(r.State)
	var input string
	if state[0] == '"' {
		if json.Unmarshal(state, &input) != nil {
			return nil, invalidRequest("cannot decode state")
		}
	} else {
		var compact bytes.Buffer
		if json.Compact(&compact, state) != nil {
			return nil, invalidRequest("cannot encode state")
		}
		input = compact.String()
	}
	body, e := json.Marshal(struct {
		Model     string     `json:"model"`
		Input     string     `json:"input"`
		Questions []question `json:"questions"`
	}{cl.model, input, qs})
	if e != nil {
		return nil, invalidRequest("cannot encode request")
	}
	if int64(len(body)) > cl.requestLimit {
		return nil, invalidRequest("request exceeds byte limit")
	}
	return body, nil
}

// Accounting is decoded first and retained even when answers are refused or
// malformed. Unknown counts stay nil; valid sibling counts survive bad fields.
func decode(body []byte, r c.Request, out *c.Response) error {
	var fields map[string]json.RawMessage
	if !utf8.Valid(body) || json.Unmarshal(body, &fields) != nil || fields == nil {
		return invalidResponse("malformed JSON response")
	}
	var model *string
	bad := json.Unmarshal(fields["model"], &model) != nil || model == nil
	if model != nil {
		out.Model = *model
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
	var answers []json.RawMessage
	if json.Unmarshal(fields["answers"], &answers) != nil || len(answers) != len(r.Questions) {
		return invalidResponse("invalid answer array")
	}
	converted := make(map[string]c.Answer, len(answers))
	for _, raw := range answers {
		var named struct {
			Name *string `json:"name"`
		}
		if json.Unmarshal(raw, &named) != nil || named.Name == nil {
			return invalidResponse("missing answer name")
		}
		id := *named.Name
		q, ok := r.Questions[id]
		if !ok {
			return invalidResponse("unexpected answer ID")
		}
		if _, seen := converted[id]; seen {
			return invalidResponse("duplicate answer ID")
		}
		a, e := decodeAnswer(raw, q)
		if e != nil {
			return e
		}
		converted[id] = a
	}
	if bad {
		return invalidResponse("invalid model or usage")
	}
	out.Answers = converted
	return nil
}

func decodeAnswer(raw json.RawMessage, q c.Question) (c.Answer, error) {
	var a struct {
		Type          string            `json:"type"`
		Probability   *float64          `json:"probability"`
		Choice        *string           `json:"choice"`
		Confidence    *float64          `json:"confidence"`
		Score         *float64          `json:"score"`
		Probabilities []json.RawMessage `json:"probabilities"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return nil, invalidResponse("malformed answer")
	}
	if a.Type == "refusal" {
		return nil, failed("provider refused classification")
	}
	switch q := q.(type) {
	case *c.BinaryQuestion:
		if a.Type == "predicate" && a.Probability != nil {
			return &c.BinaryAnswer{Probability: *a.Probability}, nil
		}
	case *c.ChoiceQuestion:
		if a.Type == "choice" && a.Choice != nil && a.Confidence != nil && len(a.Probabilities) == len(q.Choices) {
			ps := make(map[string]float64, len(q.Choices))
			for _, raw := range a.Probabilities {
				var p struct {
					Value       *string  `json:"value"`
					Probability *float64 `json:"probability"`
				}
				if json.Unmarshal(raw, &p) != nil || p.Value == nil || p.Probability == nil {
					return nil, invalidResponse("invalid candidate probability")
				}
				if _, ok := q.Choices[*p.Value]; !ok {
					return nil, invalidResponse("unexpected candidate")
				}
				if _, seen := ps[*p.Value]; seen {
					return nil, invalidResponse("duplicate candidate")
				}
				ps[*p.Value] = *p.Probability
			}
			return &c.ChoiceAnswer{Choice: *a.Choice, Confidence: *a.Confidence, Probabilities: ps}, nil
		}
	case *c.ScoreQuestion:
		if a.Type == "score" && a.Score != nil && a.Confidence != nil && len(a.Probabilities) == len(q.Levels) {
			ps := make([]float64, len(q.Levels))
			seen := make([]bool, len(q.Levels))
			for _, raw := range a.Probabilities {
				var p struct {
					Value       *int64   `json:"value"`
					Label       *string  `json:"label"`
					Probability *float64 `json:"probability"`
				}
				if json.Unmarshal(raw, &p) != nil || p.Value == nil || p.Label == nil || p.Probability == nil {
					return nil, invalidResponse("invalid level probability")
				}
				i := *p.Value
				if i < 0 || i >= int64(len(q.Levels)) || seen[i] || *p.Label != q.Levels[i] {
					return nil, invalidResponse("score levels do not match rubric")
				}
				seen[i] = true
				ps[i] = *p.Probability
			}
			return &c.ScoreAnswer{Score: *a.Score, Confidence: *a.Confidence, Probabilities: ps}, nil
		}
	}
	return nil, invalidResponse("missing or mismatched answer fields")
}
