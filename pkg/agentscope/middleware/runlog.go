package middleware

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// RunJSONL middleware (HARNESS_DESIGN A3): serializes the full reply event
// stream plus model-call records into a JSONL sink. This is the shared
// input for rundiff, the replay viewer, and offline evaluation.
//
// Production deployments should attach a redactor (prompts and tool
// payloads routinely carry secrets).
type RunJSONL struct {
	BaseMiddleware
	mu       sync.Mutex
	w        io.Writer
	redactor func(string) string
}

// NewRunJSONL creates the run-log middleware writing to w.
func NewRunJSONL(w io.Writer) *RunJSONL {
	return &RunJSONL{
		BaseMiddleware: BaseMiddleware{MiddlewareKey: "run-jsonl"},
		w:              w,
	}
}

// WithRunLogRedactor installs a redaction hook applied to every line.
func WithRunLogRedactor(fn func(string) string) func(*RunJSONL) {
	return func(r *RunJSONL) { r.redactor = fn }
}

type runLogLine struct {
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"ts"`
	Data      json.RawMessage `json:"data,omitempty"`
}

func (r *RunJSONL) writeLine(typ string, data any) {
	if r.w == nil {
		return
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	line := runLogLine{Type: typ, Timestamp: time.Now(), Data: raw}
	b, err := json.Marshal(line)
	if err != nil {
		return
	}
	s := string(b)
	if r.redactor != nil {
		s = r.redactor(s)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = io.WriteString(r.w, s+"\n")
}

// OnReply forwards the event stream, writing each event to the log.
func (r *RunJSONL) OnReply(ctx context.Context, input ReplyInput, next ReplyHandler) <-chan event.Event {
	r.writeLine("reply_input", map[string]any{
		"agent":    input.AgentName,
		"messages": len(input.Messages),
	})
	inner := next(ctx, input)
	out := make(chan event.Event, 16)
	go func() {
		defer close(out)
		for evt := range inner {
			r.writeLine("event", map[string]any{
				"event_type": string(evt.GetEventType()),
				"data":       evt,
			})
			select {
			case out <- evt:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// OnModelCall records one model-call line (request metadata + outcome).
func (r *RunJSONL) OnModelCall(ctx context.Context, input *ModelCallInput, next ModelCallHandler) (*model.ChatResponse, error) {
	start := time.Now()
	resp, err := next(ctx, input)
	rec := map[string]any{
		"agent":      firstNonEmpty(input.AgentName),
		"model":      input.ModelName,
		"messages":   len(input.Messages),
		"tools":      len(input.Tools),
		"latency_ms": time.Since(start).Milliseconds(),
	}
	if err != nil {
		rec["error"] = err.Error()
	}
	if resp != nil && resp.Usage != nil {
		rec["usage"] = resp.Usage
	}
	r.writeLine("model_call", rec)
	return resp, err
}
