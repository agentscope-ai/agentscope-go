package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/confirmation"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/runctx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

type scopedStream struct {
	ctx   context.Context
	input chan event.UserConfirmResultEvent
}

type scopedPendingConfirmation struct {
	stream  *scopedStream
	replyID string
	calls   map[string]message.ToolCallBlock
	claimed bool
}

func (s *Service) handleScopedStream(w http.ResponseWriter, r *http.Request, sess *sessionState, msg string) {
	ctx, finish, ok := s.beginScopedReply(w, r, sess)
	if !ok {
		return
	}
	defer finish()
	stream := &scopedStream{ctx: ctx, input: make(chan event.UserConfirmResultEvent, 1)}
	ctx = confirmation.WithInput(ctx, sess.agent, stream.input)
	ch, err := sess.agent.ReplyStream(ctx, msg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() {
		s.mu.Lock()
		if sess.pending != nil && sess.pending.stream == stream {
			sess.pending = nil
		}
		s.mu.Unlock()
	}()
	sse, err := NewSSEWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for evt := range ch {
		if req, ok := evt.(event.RequireUserConfirmEvent); ok {
			pending := &scopedPendingConfirmation{stream: stream, replyID: req.ReplyID, calls: make(map[string]message.ToolCallBlock, len(req.ToolCalls))}
			for _, tc := range req.ToolCalls {
				if tc.ID == "" {
					_ = writeScopedEvent(sse, "error", map[string]string{"error": "invalid pending confirmation"})
					return
				}
				if _, duplicate := pending.calls[tc.ID]; duplicate {
					_ = writeScopedEvent(sse, "error", map[string]string{"error": "invalid pending confirmation"})
					return
				}
				// Keep the server's complete call; the client supplies only ID/decision.
				pending.calls[tc.ID] = tc
			}
			if len(pending.calls) == 0 {
				_ = writeScopedEvent(sse, "error", map[string]string{"error": "invalid pending confirmation"})
				return
			}
			s.mu.Lock()
			sess.pending = pending
			s.mu.Unlock()
		}
		if err := writeScopedEvent(sse, string(evt.GetEventType()), evt); err != nil {
			return
		}
	}
}

// Scoped streams must observe I/O failures to cancel their reply-owned input.
// The legacy SSEWriter/public handler behavior is kept separate.
func writeScopedEvent(sse *SSEWriter, name string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	frame := "event: " + name + "\ndata: " + string(b) + "\n\n"
	n, err := sse.w.Write([]byte(frame))
	if err != nil {
		return err
	}
	if n != len(frame) {
		return io.ErrShortWrite
	}
	sse.flusher.Flush()
	return nil
}

// A session owns one scoped reply until all its accepted cores have returned.
// The legacy Agent API permits concurrent direct replies; sharing pending input
// between them would let an overlapping stream overwrite the current request.
func (s *Service) beginScopedReply(w http.ResponseWriter, r *http.Request, sess *sessionState) (context.Context, func(), bool) {
	s.mu.Lock()
	if sess.activeReply {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "session reply already active")
		return nil, nil, false
	}
	sess.activeReply = true
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(r.Context())
	ctx, cores := runctx.New(ctx)
	return ctx, func() {
		cancel()
		cores.Close()
		s.mu.Lock()
		sess.pending = nil
		sess.activeReply = false
		s.mu.Unlock()
	}, true
}

func (s *Service) handleScopedConfirmation(w http.ResponseWriter, r *http.Request, sess *sessionState, req ConfirmRequest) {
	s.mu.Lock()
	p := sess.pending
	if p == nil || p.claimed || p.stream.ctx.Err() != nil || len(req.ToolCalls) == 0 || len(req.ToolCalls) != len(p.calls) {
		s.mu.Unlock()
		writeError(w, http.StatusBadRequest, "no matching pending confirmation")
		return
	}
	results := make([]event.ConfirmResult, 0, len(req.ToolCalls))
	seen := make(map[string]bool, len(req.ToolCalls))
	for _, answer := range req.ToolCalls {
		tc, ok := p.calls[answer.ID]
		if !ok || seen[answer.ID] {
			s.mu.Unlock()
			writeError(w, http.StatusBadRequest, "no matching pending confirmation")
			return
		}
		seen[answer.ID] = true
		results = append(results, event.ConfirmResult{Confirmed: answer.Confirmed, ToolCall: tc})
	}
	p.claimed = true
	s.mu.Unlock()
	accepted := false
	defer func() {
		s.mu.Lock()
		if sess.pending == p {
			if accepted || p.stream.ctx.Err() != nil {
				sess.pending = nil
			} else {
				p.claimed = false
			}
		}
		s.mu.Unlock()
	}()
	if requestCanceled(w, r) {
		return
	}
	result := event.NewUserConfirmResultEvent(p.replyID, results)
	// A canceled stream's buffered response is never reused by a later reply.
	// No session lock, raw Agent sender or detached goroutine owns this wait.
	select {
	case <-p.stream.ctx.Done():
		writeError(w, http.StatusBadRequest, "no matching pending confirmation")
	case <-r.Context().Done():
		requestCanceled(w, r)
	case p.stream.input <- result:
		accepted = true
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
