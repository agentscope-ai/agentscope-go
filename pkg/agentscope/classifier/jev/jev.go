// Package jev implements the experimental TypeSafe System One HTTP classifier.
// Hosts must authorize and project all state and question data before Classify.
package jev

import (
	"bytes"
	"context"
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
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/internal/systemone"
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
	return cl.ClassifyWithDispatch(ctx, r, c.DispatchLimits{}, nil)
}

// Target returns the actual endpoint and configured requested version.
func (cl *Client) Target() (endpoint, model string) { return cl.endpoint, cl.model }

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
	parseErr := systemone.Decode(body, r, out, invalidResponse)
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

func (cl *Client) encode(r c.Request) ([]byte, error) {
	for _, q := range r.Questions {
		switch q := q.(type) {
		case *c.ChoiceQuestion:
			if len(q.Choices) > 255 {
				return nil, invalidRequest("Choice supports at most 255 candidates")
			}
		case *c.ScoreQuestion:
			if len(q.Levels) < 2 || len(q.Levels) > 10 {
				return nil, invalidRequest("Score requires 2 to 10 levels")
			}
		}
	}
	body, err := systemone.Encode(cl.model, r, invalidRequest)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > cl.requestLimit {
		return nil, invalidRequest("request exceeds byte limit")
	}
	return body, nil
}
