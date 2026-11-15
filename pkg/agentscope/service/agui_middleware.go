package service

import (
	"net/http"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
)

// AGUIMiddleware wraps an SSE handler to automatically convert agent events
// to AG-UI protocol format before sending to clients.
type AGUIMiddleware struct {
	next http.Handler
}

// NewAGUIMiddleware creates an AG-UI protocol conversion middleware.
func NewAGUIMiddleware(next http.Handler) *AGUIMiddleware {
	return &AGUIMiddleware{next: next}
}

// AGUISSEWriter wraps an SSEWriter and converts events to AG-UI format.
type AGUISSEWriter struct {
	sse *SSEWriter
}

// NewAGUISSEWriter creates an AG-UI SSE writer.
func NewAGUISSEWriter(w http.ResponseWriter) (*AGUISSEWriter, error) {
	sse, err := NewSSEWriter(w)
	if err != nil {
		return nil, err
	}
	return &AGUISSEWriter{sse: sse}, nil
}

// WriteEvent converts an agent event to AG-UI format and writes it.
func (a *AGUISSEWriter) WriteEvent(eventName string, evt event.Event) error {
	ae := ConvertToAGUI(evt)
	if ae == nil {
		return nil
	}
	return a.sse.WriteEvent(ae.Type, ae)
}
