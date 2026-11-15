// Package service provides an HTTP agent service with SSE streaming.
//
// The service exposes REST endpoints for session management, chat interaction,
// and model/credential administration. Chat responses are streamed as
// Server-Sent Events (SSE).
package service

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// SSEWriter writes Server-Sent Events to an http.ResponseWriter.
type SSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// NewSSEWriter wraps a ResponseWriter for SSE output.
// Returns an error if the writer does not support flushing.
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming not supported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()
	return &SSEWriter{w: w, flusher: flusher}, nil
}

// WriteEvent sends a named SSE event with JSON data.
func (s *SSEWriter) WriteEvent(eventType string, data any) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("sse: marshal: %w", err)
	}
	if eventType != "" {
		fmt.Fprintf(s.w, "event: %s\n", eventType)
	}
	fmt.Fprintf(s.w, "data: %s\n\n", jsonData)
	s.flusher.Flush()
	return nil
}

// WriteData sends an SSE event with just a data field (no event name).
func (s *SSEWriter) WriteData(data any) error {
	return s.WriteEvent("", data)
}

// WriteComment sends an SSE comment (used for keep-alive pings).
func (s *SSEWriter) WriteComment(comment string) {
	fmt.Fprintf(s.w, ": %s\n\n", comment)
	s.flusher.Flush()
}
