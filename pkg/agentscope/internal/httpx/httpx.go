package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	defaultMaxAttempts = 3
	defaultBaseBackoff = 200 * time.Millisecond
	maxBackoff         = 30 * time.Second
)

// DoJSONRequest sends a JSON request and decodes a JSON response with basic retries.
// It is intended for outbound calls to LLM providers and other HTTP JSON APIs.
func DoJSONRequest(
	ctx context.Context,
	client *http.Client,
	method string,
	url string,
	reqBody any,
	respBody any,
	headers map[string]string,
) error {
	if client == nil {
		client = http.DefaultClient
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("httpx: marshal request: %w", err)
	}

	var lastErr error
	var retryAfter time.Duration // server-requested delay carried from the previous attempt

	for attempt := 0; attempt < defaultMaxAttempts; attempt++ {
		if attempt > 0 {
			// Full-jitter exponential backoff, overridden by a larger server
			// Retry-After. Cancellable so a canceled context is not slept through.
			delay := backoffWithJitter(attempt)
			if retryAfter > delay {
				delay = retryAfter
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		retryAfter = 0

		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("httpx: new request: %w", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		logrus.WithFields(logrus.Fields{
			"method":  method,
			"url":     redactURL(url),
			"attempt": attempt + 1,
		}).Debug("httpx: sending JSON request")

		resp, err := client.Do(req)
		if err != nil {
			// Retry on temporary network errors.
			if isRetryableError(err) && attempt < defaultMaxAttempts-1 {
				logrus.WithError(err).WithFields(logrus.Fields{
					"method":  method,
					"url":     redactURL(url),
					"attempt": attempt + 1,
				}).Warn("httpx: retrying after network error")
				lastErr = err
				continue
			}
			return fmt.Errorf("httpx: do request: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("httpx: read response: %w", err)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// Retry 429 (Too Many Requests) and 5xx, honoring Retry-After.
			if isRetryableStatus(resp.StatusCode) && attempt < defaultMaxAttempts-1 {
				retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
				logrus.WithFields(logrus.Fields{
					"method":     method,
					"url":        redactURL(url),
					"statusCode": resp.StatusCode,
					"attempt":    attempt + 1,
					"retryAfter": retryAfter,
				}).Warn("httpx: retryable status, will retry")
				lastErr = fmt.Errorf("httpx: unexpected status %d: %s", resp.StatusCode, string(body))
				continue
			}
			return fmt.Errorf("httpx: unexpected status %d: %s", resp.StatusCode, string(body))
		}

		if respBody != nil {
			if err := json.Unmarshal(body, respBody); err != nil {
				return fmt.Errorf("httpx: decode response: %w", err)
			}
		}
		logrus.WithFields(logrus.Fields{
			"method":     method,
			"url":        redactURL(url),
			"statusCode": resp.StatusCode,
		}).Debug("httpx: request succeeded")
		return nil
	}

	if lastErr != nil {
		logrus.WithError(lastErr).WithFields(logrus.Fields{
			"method": method,
			"url":    redactURL(url),
		}).Error("httpx: request failed after retries")
		return lastErr
	}
	return fmt.Errorf("httpx: request failed after %d attempts", defaultMaxAttempts)
}

func isRetryableError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

// isRetryableStatus reports whether an HTTP status code warrants a retry.
func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// backoffWithJitter returns a full-jitter exponential backoff for the given
// attempt (1-based), capped at maxBackoff.
func backoffWithJitter(attempt int) time.Duration {
	ceil := defaultBaseBackoff * time.Duration(1<<attempt)
	if ceil > maxBackoff || ceil <= 0 {
		ceil = maxBackoff
	}
	return time.Duration(rand.Int63n(int64(ceil)))
}

// parseRetryAfter parses a Retry-After header value, which may be either an
// integer number of seconds or an HTTP-date. Returns 0 if absent/invalid.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
