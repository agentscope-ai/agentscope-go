package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// sseDeltaLine builds one Responses-API text delta SSE line.
func sseDeltaLine(delta string) string {
	return fmt.Sprintf("data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n", delta)
}

func sseCompletedLine() string {
	return "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5}}}\n"
}

func newResponsesStreamModel(t *testing.T, handler http.HandlerFunc) ChatModel {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	m, err := NewOpenAIResponseModel(&OpenAIResponseConfig{
		APIKey:  "k",
		Model:   "gpt-4.1",
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewOpenAIResponseModel: %v", err)
	}
	return m
}

// Upstream #2349: when the consumer abandons the stream, the producer must
// stop deterministically instead of blocking forever on a full buffer.
func TestOpenAIResponseStream_CancelStopsProducer(t *testing.T) {
	m := newResponsesStreamModel(t, func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("no flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// Far more deltas than the 32-slot channel buffer.
		for i := 0; i < 200; i++ {
			fmt.Fprint(w, sseDeltaLine("x"))
			fl.Flush()
			time.Sleep(time.Millisecond)
		}
		fmt.Fprint(w, sseCompletedLine())
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := m.ChatStream(ctx, []*message.Msg{message.UserMsg("u", "hi")})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	// Consume one event, then abandon.
	if _, ok := <-ch; !ok {
		t.Fatal("expected at least one response")
	}
	cancel()

	select {
	case _, ok := <-ch:
		for ok {
			_, ok = <-ch
		}
		// channel closed — producer stopped deterministically
	case <-time.After(5 * time.Second):
		t.Fatal("producer did not stop after ctx cancel (upstream #2349): stream leaked")
	}
}

// Upstream #2349: a mid-stream read error must surface as a final error
// response, not end silently (and not be swallowed into logs).
func TestOpenAIResponseStream_ScanErrorSurfaces(t *testing.T) {
	m := newResponsesStreamModel(t, func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseDeltaLine("hello "))
		fl.Flush()
		// A line bigger than the 1 MiB scanner buffer → bufio.ErrTooLong.
		fmt.Fprint(w, "data: "+strings.Repeat("x", 2*1024*1024)+"\n")
		fl.Flush()
	})

	ch, err := m.ChatStream(context.Background(), []*message.Msg{message.UserMsg("u", "hi")})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var last ChatResponse
	sawLast := false
	for resp := range ch {
		if resp.IsLast {
			last = resp
			sawLast = true
		}
	}
	if !sawLast {
		t.Fatal("no final IsLast response emitted")
	}
	if last.Error == nil {
		t.Error("final response should carry the stream error")
	}
}

// Upstream #2349: if the stream ends without response.completed (truncated
// connection), consumers must still receive a final IsLast response with the
// accumulated content.
func TestOpenAIResponseStream_TruncatedStillEmitsFinal(t *testing.T) {
	m := newResponsesStreamModel(t, func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseDeltaLine("partial"))
		fl.Flush()
		// Connection closes without [DONE]/response.completed.
	})

	ch, err := m.ChatStream(context.Background(), []*message.Msg{message.UserMsg("u", "hi")})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var last ChatResponse
	sawLast := false
	for resp := range ch {
		if resp.IsLast {
			last = resp
			sawLast = true
		}
	}
	if !sawLast {
		t.Fatal("no final IsLast response emitted for a truncated stream")
	}
	var text string
	for _, b := range last.Content {
		if tb, ok := b.(message.TextBlock); ok {
			text += tb.Text
		}
	}
	if !strings.Contains(text, "partial") {
		t.Errorf("final content = %q, want accumulated text", text)
	}
}
