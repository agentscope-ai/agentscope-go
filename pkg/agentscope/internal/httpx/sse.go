package httpx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"
)

// SSEEvent represents a single Server-Sent Event.
type SSEEvent struct {
	Event string // event type (empty means "message")
	Data  string // event data payload
	ID    string // optional event ID
	Err   error  // non-nil on a terminal stream failure (scanner/transport error)
}

// DoSSERequest sends an HTTP request and returns a channel that yields parsed SSE events.
// The channel is closed when the stream ends (either normally or via context cancellation).
// The caller must consume all events from the channel to avoid goroutine leaks.
func DoSSERequest(
	ctx context.Context,
	client *http.Client,
	method string,
	url string,
	reqBody any,
	headers map[string]string,
) (<-chan SSEEvent, error) {
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
	}).Debug("httpx: starting SSE stream")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpx: do request: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("httpx: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	ch := make(chan SSEEvent, 16)
	go parseSSEStream(ctx, resp.Body, ch)
	return ch, nil
}

func parseSSEStream(ctx context.Context, body io.ReadCloser, ch chan<- SSEEvent) {
	defer close(ch)
	defer body.Close()

	scanner := bufio.NewScanner(body)
	// Increase buffer for large SSE payloads (e.g. tool call arguments)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var event SSEEvent
	var dataBuf strings.Builder

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Text()

		// Empty line = dispatch event
		if line == "" {
			if dataBuf.Len() > 0 {
				event.Data = dataBuf.String()
				// Trim trailing newline added by multi-line data
				event.Data = strings.TrimSuffix(event.Data, "\n")
				select {
				case ch <- event:
				case <-ctx.Done():
					return
				}
			}
			event = SSEEvent{}
			dataBuf.Reset()
			continue
		}

		// Comment lines start with ':'
		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value := parseSSELine(line)
		switch field {
		case "event":
			event.Event = value
		case "data":
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(value)
		case "id":
			event.ID = value
		}
	}

	// Flush any remaining buffered event
	if dataBuf.Len() > 0 {
		event.Data = strings.TrimSuffix(dataBuf.String(), "\n")
		select {
		case ch <- event:
		case <-ctx.Done():
			return
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		logrus.WithError(err).Warn("httpx: SSE stream scanner error")
		// Surface the failure as a terminal event so consumers can mark the
		// stream errored instead of treating the truncated output as complete.
		select {
		case ch <- SSEEvent{Err: err}:
		case <-ctx.Done():
		}
	}
}

func parseSSELine(line string) (field, value string) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return line, ""
	}
	field = line[:idx]
	value = line[idx+1:]
	// Per SSE spec: if value starts with a space, remove it
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value
}
