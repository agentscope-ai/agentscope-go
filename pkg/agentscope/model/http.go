package model

import (
	"context"
	"net/http"
	"time"
)

// defaultModelTimeout is the HTTP timeout applied to all model API calls
// when the caller does not supply a custom http.Client.
const defaultModelTimeout = 60 * time.Second

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		// Cancellation may be ready at the same time as the timer.
		return ctx.Err()
	}
}

// ClientOptions provides production-grade HTTP client configuration for model
// providers, mirroring Python's client_kwargs. Use it to set proxy, custom
// headers, or timeouts without constructing an *http.Client manually.
type ClientOptions struct {
	Timeout        time.Duration
	DefaultHeaders map[string]string
	Transport      http.RoundTripper
}

// defaultHTTPClient returns an *http.Client suitable for LLM API calls.
// If a non-nil client is provided it is returned unchanged; otherwise a
// new client is built from ClientOptions (or defaults).
func defaultHTTPClient(c *http.Client, opts *ClientOptions) *http.Client {
	if c != nil {
		return c
	}
	timeout := defaultModelTimeout
	var transport http.RoundTripper
	if opts != nil {
		if opts.Timeout > 0 {
			timeout = opts.Timeout
		}
		if opts.Transport != nil {
			transport = opts.Transport
		}
	}
	client := &http.Client{Timeout: timeout}
	if transport != nil {
		client.Transport = transport
	}
	return client
}

// mergeHeaders returns a new header map with defaults merged under explicit
// values. Explicit values take precedence.
func mergeHeaders(explicit map[string]string, defaults map[string]string) map[string]string {
	if len(defaults) == 0 {
		return explicit
	}
	merged := make(map[string]string, len(explicit)+len(defaults))
	for k, v := range defaults {
		merged[k] = v
	}
	for k, v := range explicit {
		merged[k] = v
	}
	return merged
}
