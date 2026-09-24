package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type managedTransport struct{ transport *http.Transport }

func (t *managedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.transport.RoundTrip(r)
}
func (t *managedTransport) CloseIdleConnections() { t.transport.CloseIdleConnections() }

// NewHTTPClient copies a client with an owned standard Transport and disabled
// redirects. Custom RoundTrippers/protocol handlers are rejected: their hidden
// retries cannot be accounted for. Configure clients before sharing them.
// The owner should call CloseIdleConnections when this client is no longer used.
func NewHTTPClient(client *http.Client) (*http.Client, error) {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if t, ok := transport.(*managedTransport); ok {
		clone.Transport = t
	} else {
		t, ok := transport.(*http.Transport)
		if !ok || t == nil {
			return nil, fmt.Errorf("inference: custom HTTP transport is not supported")
		}
		owned := t.Clone()
		if len(owned.TLSNextProto) > 0 {
			return nil, fmt.Errorf("inference: custom HTTP protocol handlers are not supported")
		}
		clone.Transport = &managedTransport{transport: owned}
	}
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &clone, nil
}

func (op *Operation) prepare(client *http.Client, method, address string, body any, headers map[string]string, stream bool) (*http.Client, *http.Request, []byte, error) {
	if method != http.MethodPost {
		return nil, nil, nil, fmt.Errorf("inference: only JSON POST is managed")
	}
	u, err := url.Parse(address)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("inference: invalid request URL")
	}
	base, _ := url.Parse(op.deployment.cfg.Descriptor.Endpoint)
	if u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || u.Fragment != "" || (u.Path != base.Path && !strings.HasPrefix(u.Path, strings.TrimRight(base.Path, "/")+"/")) {
		return nil, nil, nil, fmt.Errorf("inference: request target differs from deployment")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("inference: encode request: %w", err)
	}
	client, err = NewHTTPClient(client)
	if err != nil {
		return nil, nil, nil, err
	}
	if stream {
		client.Timeout = 0
	}
	req, err := http.NewRequest(method, address, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("inference: invalid request")
	}
	// A consumed body must never be replayed inside Transport. This matters for
	// HTTP/2 REFUSED_STREAM as well as HTTP/1 connection retries. Attempts recreate
	// their body explicitly outside Transport, after fresh admission.
	req.GetBody = nil
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return client, req, payload, nil
}

// DoJSON is the managed transport entry used by supported adapters. Retries
// include only 429/5xx and timeout errors; decoding errors are never retried.
func (op *Operation) DoJSON(ctx context.Context, client *http.Client, method, address string, body, out any, headers map[string]string) error {
	temporary := !isManagedClient(client)
	client, prototype, payload, err := op.prepare(client, method, address, body, headers, false)
	if err != nil {
		return err
	}
	if temporary {
		defer client.CloseIdleConnections()
	}
	var retryAfter time.Duration
	for n := 0; ; n++ {
		if n > 0 {
			if err := op.backoff(ctx, n, retryAfter); err != nil {
				return err
			}
		}
		id, release, requestCtx, cancel, stop, err := op.next(ctx)
		if err != nil {
			return err
		}
		req := prototype.Clone(requestCtx)
		req.Body = io.NopCloser(bytes.NewReader(payload))
		req.GetBody = nil
		resp, sendErr := client.Do(req)
		sendErr = safeRequestError(sendErr)
		var data []byte
		status := 0
		retryAfter = 0
		if resp != nil {
			status = resp.StatusCode
			retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
			data, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if sendErr == nil {
				sendErr = err
			}
		}
		if sendErr == nil && requestCtx.Err() != nil {
			sendErr = requestCtx.Err()
		}
		stop()
		cancel()
		release()
		sample := decodeUsage(op.deployment.cfg.Descriptor.API, data)
		outcome := "success"
		retry := false
		if sendErr != nil {
			outcome = errorOutcome(sendErr)
			var ne net.Error
			retry = (status == 429 || status >= 500 || errors.As(sendErr, &ne) && ne.Timeout()) && !errors.Is(sendErr, context.Canceled)
		} else if status < 200 || status >= 300 {
			outcome = "http_error"
			sendErr = statusError(status, data)
			retry = status == 429 || status >= 500
		} else if out != nil {
			if err := json.Unmarshal(data, out); err != nil {
				sendErr = fmt.Errorf("inference: decode response: %w", err)
				outcome = "decode_error"
			}
		}
		op.deployment.ledger.finish(id, &sample.usage, sample.known(), status, outcome, op.deployment.cfg.Price)
		if sendErr == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !retry || !op.hasAttempts() {
			return sendErr
		}
	}
}

// OpenStream admits and retries stream setup only. Successful setup transfers
// response body/parser completion to the adapter; Start's finish retains the
// permit until StreamTransportDone and all model producers have completed.
func (op *Operation) OpenStream(ctx context.Context, client *http.Client, method, address string, body any, headers map[string]string) (*http.Response, error) {
	temporary := !isManagedClient(client)
	client, prototype, payload, err := op.prepare(client, method, address, body, headers, true)
	if err != nil {
		return nil, err
	}
	if temporary {
		defer client.CloseIdleConnections()
	}
	var retryAfter time.Duration
	for n := 0; ; n++ {
		if n > 0 {
			if err := op.backoff(ctx, n, retryAfter); err != nil {
				return nil, err
			}
		}
		id, release, requestCtx, cancel, stop, err := op.next(ctx)
		if err != nil {
			return nil, err
		}
		req := prototype.Clone(requestCtx)
		req.Body = io.NopCloser(bytes.NewReader(payload))
		req.GetBody = nil
		resp, sendErr := client.Do(req)
		sendErr = safeRequestError(sendErr)
		if sendErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			op.mu.Lock()
			op.stream = &streamAttempt{id: id, release: release, cancel: cancel, stop: stop, done: make(chan struct{}), status: resp.StatusCode}
			op.mu.Unlock()
			return resp, nil
		}
		var data []byte
		status := 0
		retryAfter = 0
		if resp != nil {
			status = resp.StatusCode
			retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
			data, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if sendErr == nil {
				sendErr = err
			}
		}
		stop()
		cancel()
		release()
		retry := false
		outcome := "http_error"
		if sendErr != nil {
			outcome = errorOutcome(sendErr)
			var ne net.Error
			retry = (status == 429 || status >= 500 || errors.As(sendErr, &ne) && ne.Timeout()) && !errors.Is(sendErr, context.Canceled)
		} else {
			sendErr = statusError(status, data)
			retry = status == 429 || status >= 500
		}
		sample := decodeUsage(op.deployment.cfg.Descriptor.API, data)
		op.deployment.ledger.finish(id, &sample.usage, sample.known(), status, outcome, op.deployment.cfg.Price)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !retry || !op.hasAttempts() {
			return nil, sendErr
		}
	}
}
func (op *Operation) hasAttempts() bool {
	op.mu.Lock()
	defer op.mu.Unlock()
	return !op.finished && op.attempts < op.deployment.cfg.MaxAttempts
}
func (op *Operation) backoff(ctx context.Context, n int, retryAfter time.Duration) error {
	delay := op.deployment.cfg.Backoff
	for j := 0; j < n && delay < 15*time.Second; j++ {
		delay *= 2
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	delay = time.Duration(rand.Int64N(int64(delay) + 1))
	if retryAfter > delay {
		delay = retryAfter
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-op.deployment.pool.ctx.Done():
		return ErrClosed
	case <-timer.C:
		return nil
	}
}
func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		const maxSeconds = int64((1<<63 - 1) / time.Second)
		if seconds > maxSeconds {
			return time.Duration(1<<63 - 1)
		}
		return time.Duration(seconds) * time.Second
	}
	if len(raw) > 0 {
		digits := true
		for _, r := range raw {
			if r < '0' || r > '9' {
				digits = false
				break
			}
		}
		if digits {
			return time.Duration(1<<63 - 1)
		}
	}
	if date, err := http.ParseTime(raw); err == nil {
		if delay := time.Until(date); delay > 0 {
			return delay
		}
	}
	return 0
}

func isManagedClient(client *http.Client) bool {
	if client == nil {
		return false
	}
	_, ok := client.Transport.(*managedTransport)
	return ok
}
func safeRequestError(err error) error {
	var requestErr *url.Error
	if errors.As(err, &requestErr) {
		return &url.Error{Op: requestErr.Op, URL: "managed deployment", Err: requestErr.Err}
	}
	return err
}

// HTTPStatusError exposes the status and a bounded compatibility hint. Provider
// bodies can contain prompts/credentials and are not copied into error text.
type HTTPStatusError struct {
	StatusCode        int
	CompatibilityHint string
}

func (e *HTTPStatusError) Error() string {
	if e.CompatibilityHint != "" {
		return fmt.Sprintf("inference: HTTP status %d: %s rejected", e.StatusCode, e.CompatibilityHint)
	}
	return fmt.Sprintf("inference: HTTP status %d", e.StatusCode)
}
func statusError(status int, body []byte) error {
	e := &HTTPStatusError{StatusCode: status}
	if status == 400 || status == 422 {
		message := strings.ToLower(string(body))
		if strings.Contains(message, "tool_choice") {
			e.CompatibilityHint = "tool_choice"
		} else if strings.Contains(message, "thinking") {
			e.CompatibilityHint = "thinking"
		}
	}
	return e
}
