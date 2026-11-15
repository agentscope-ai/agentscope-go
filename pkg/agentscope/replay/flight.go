package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/fsutil"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
)

// Flight-recorder extensions for the recording middleware (HARNESS_DESIGN
// A1): bounded retention, per-record size caps with summary fallback, and
// automatic dump of the tape plus an event tail when a reply ends in error.
// Everything builds on the single replay.Tape/Entry format — no parallel
// record schema.

// RecorderOption configures NewRecorder.
type RecorderOption func(*Middleware)

// WithRingLimit bounds the tape to maxEntries most recent entries (0 =
// unbounded). maxBytes additionally bounds the approximate serialized size
// (0 = no byte cap); oldest entries are dropped first.
func WithRingLimit(maxEntries, maxBytes int) RecorderOption {
	return func(m *Middleware) {
		m.ringEntries = maxEntries
		m.ringBytes = maxBytes
	}
}

// WithRecordSizeLimit caps a single record's serialized messages; oversized
// inputs are stored as a structured summary instead (never verbatim), so one
// huge context cannot evict the whole ring.
func WithRecordSizeLimit(maxRecordBytes int) RecorderOption {
	return func(m *Middleware) { m.recordLimit = maxRecordBytes }
}

// WithDumpOnError writes a flight file (tape + event tail, JSONL) to dir
// whenever a reply ends in error. Atomic writes; filenames are
// flight-<reply_id>-<unix>.jsonl.
func WithDumpOnError(dir string) RecorderOption {
	return func(m *Middleware) { m.dumpDir = dir }
}

// WithRedactor installs a redaction hook applied to the whole flight file
// content before it is written (production deployments should always set
// one: prompts and tool payloads routinely carry secrets).
func WithRedactor(fn func(string) string) RecorderOption {
	return func(m *Middleware) { m.redactor = fn }
}

// summarizeMessages renders a compact, schema-stable summary of messages
// for oversized records.
func summarizeMessages(msgs []*message.Msg) json.RawMessage {
	out := make([]map[string]any, 0, len(msgs))
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		text := msg.GetTextContent(" ")
		preview := ""
		if text != nil {
			preview = *text
			if len(preview) > 200 {
				preview = preview[:200] + "...(truncated)"
			}
		}
		out = append(out, map[string]any{
			"summarized": true,
			"role":       string(msg.Role),
			"name":       msg.Name,
			"blocks":     len(msg.Content),
			"preview":    preview,
		})
	}
	b, _ := json.Marshal(out)
	return b
}

// appendEntryLocked adds an entry enforcing ring caps. Caller holds m.mu.
func (m *Middleware) appendEntryLocked(entry *Entry) {
	entry.Index = m.nextIndex
	m.nextIndex++
	entryBytes := approxEntryBytes(entry)
	m.tape.Entries = append(m.tape.Entries, *entry)
	m.totalBytes += entryBytes

	if m.ringEntries > 0 {
		for len(m.tape.Entries) > m.ringEntries {
			m.dropHeadLocked()
		}
	}
	if m.ringBytes > 0 {
		for len(m.tape.Entries) > 1 && m.totalBytes > int64(m.ringBytes) {
			m.dropHeadLocked()
		}
	}
}

// dropHeadLocked evicts the oldest entry. Caller holds m.mu.
func (m *Middleware) dropHeadLocked() {
	if len(m.tape.Entries) == 0 {
		return
	}
	m.totalBytes -= approxEntryBytes(&m.tape.Entries[0])
	m.tape.Entries = m.tape.Entries[1:]
}

func approxEntryBytes(e *Entry) int64 {
	return int64(len(e.Messages) + len(e.Tools) + len(e.Response) + len(e.Error))
}

// OnReply extends the base hook in record mode: it observes the event
// stream to capture the reply ID and a tail of recent events, and dumps a
// flight file when the reply ends in error.
func (m *Middleware) OnReply(ctx context.Context, input middleware.ReplyInput, next middleware.ReplyHandler) <-chan event.Event {
	ch := next(ctx, input)
	if m.mode != ModeRecord || m.dumpDir == "" {
		return ch
	}

	out := make(chan event.Event, 16)
	go func() {
		defer close(out)

		const tailCap = 50
		tail := make([]event.Event, 0, tailCap)
		replyID := ""
		sawEnd := false
		sawError := false

		for evt := range ch {
			switch e := evt.(type) {
			case event.ReplyStartEvent:
				replyID = e.ReplyID
			case event.ReplyEndEvent:
				sawEnd = true
			case event.CustomEvent:
				if strings.Contains(e.Name, "error") {
					sawError = true
				}
			}
			tail = append(tail, evt)
			if len(tail) > tailCap {
				tail = tail[1:]
			}
			select {
			case out <- evt:
			case <-ctx.Done():
				return
			}
		}

		if sawError || !sawEnd {
			reason := "reply ended with an error event"
			if !sawError {
				reason = "reply stream ended without ReplyEndEvent"
			}
			m.dumpFlightFile(input.AgentName, replyID, reason, tail)
		}
	}()
	return out
}

// dumpFlightFile writes tape + event tail as JSONL, redacted when a
// redactor is configured.
func (m *Middleware) dumpFlightFile(agentName, replyID, reason string, tail []event.Event) {
	m.mu.Lock()
	entries := make([]Entry, len(m.tape.Entries))
	copy(entries, m.tape.Entries)
	m.mu.Unlock()

	id := replyID
	if id == "" {
		id = "unknown"
	}

	var sb strings.Builder
	meta, _ := json.Marshal(map[string]any{
		"type":       "flight_meta",
		"reply_id":   id,
		"agent_name": agentName,
		"dumped_at":  time.Now().Format(time.RFC3339Nano),
		"reason":     reason,
	})
	sb.Write(meta)
	sb.WriteByte('\n')

	for i := range entries {
		line, err := json.Marshal(struct {
			Type string `json:"type"`
			Entry
		}{"model_call", entries[i]})
		if err != nil {
			continue
		}
		sb.Write(line)
		sb.WriteByte('\n')
	}

	for _, evt := range tail {
		line, err := json.Marshal(map[string]any{
			"type":       "event",
			"event_type": string(evt.GetEventType()),
			"data":       evt,
		})
		if err != nil {
			continue
		}
		sb.Write(line)
		sb.WriteByte('\n')
	}

	content := sb.String()
	if m.redactor != nil {
		content = m.redactor(content)
	}

	name := fmt.Sprintf("flight-%s-%d.jsonl", id, time.Now().UnixNano())
	path := filepath.Join(m.dumpDir, name)
	if err := fsutil.WriteFileAtomic(path, []byte(content), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "replay: flight dump failed: %v\n", err)
	}
}
