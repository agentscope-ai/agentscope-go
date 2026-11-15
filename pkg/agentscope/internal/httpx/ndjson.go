package httpx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/sirupsen/logrus"
)

// NDJSONEvent represents a single line-delimited JSON event.
type NDJSONEvent struct {
	Data []byte // one raw JSON line, without the trailing newline
	Err  error  // non-nil on a terminal stream failure (scanner/transport error)
}

// DoNDJSONRequest sends an HTTP request and returns a channel that yields one
// event per non-empty response line (newline-delimited JSON). The channel is
// closed when the stream ends (either normally or via context cancellation).
// The caller must consume all events from the channel to avoid goroutine leaks.
func DoNDJSONRequest(
	ctx context.Context,
	client *http.Client,
	method string,
	url string,
	reqBody any,
	headers map[string]string,
) (<-chan NDJSONEvent, error) {
	if client == nil {
		client = http.DefaultClient
	}

	// A streaming request must not be bounded by the client's whole-request
	// Timeout (that caps total wall-clock and truncates long generations). Clone
	// the client with Timeout=0; the stream's lifetime is governed by ctx instead.
	if client.Timeout != 0 {
		clone := *client
		clone.Timeout = 0
		client = &clone
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("httpx: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("httpx: new request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	logrus.WithFields(logrus.Fields{
		"method": method,
		"url":    redactURL(url),
	}).Debug("httpx: starting NDJSON stream")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpx: do request: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("httpx: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	ch := make(chan NDJSONEvent, 16)
	go parseNDJSONStream(ctx, resp.Body, ch)
	return ch, nil
}

func parseNDJSONStream(ctx context.Context, body io.ReadCloser, ch chan<- NDJSONEvent) {
	defer close(ch)
	defer body.Close()

	scanner := bufio.NewScanner(body)
	// Increase buffer for large NDJSON payloads
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		select {
		case ch <- NDJSONEvent{Data: bytes.Clone(line)}:
		case <-ctx.Done():
			return
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		logrus.WithError(err).Warn("httpx: NDJSON stream scanner error")
		// Surface the failure as a terminal event so consumers can mark the
		// stream errored instead of treating the truncated output as complete.
		select {
		case ch <- NDJSONEvent{Err: err}:
		case <-ctx.Done():
		}
	}
}
