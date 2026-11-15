package httpx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func collectNDJSON(ch <-chan NDJSONEvent) []NDJSONEvent {
	var out []NDJSONEvent
	for evt := range ch {
		out = append(out, evt)
	}
	return out
}

// TestDoNDJSONRequest_MultiLineStream proves each non-empty line is delivered
// as one complete event (no data: prefix, no [DONE] sentinel, no multi-line
// joining) and that empty lines are skipped.
func TestDoNDJSONRequest_MultiLineStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, "{\"a\":1}\n\n{\"b\":2}\n   \n{\"c\":3}\n")
	}))
	defer srv.Close()

	ch, err := DoNDJSONRequest(context.Background(), srv.Client(), http.MethodPost, srv.URL, map[string]any{"k": "v"}, nil)
	if err != nil {
		t.Fatalf("DoNDJSONRequest: %v", err)
	}

	events := collectNDJSON(ch)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	want := []string{`{"a":1}`, `{"b":2}`, `{"c":3}`}
	for i, evt := range events {
		if evt.Err != nil {
			t.Fatalf("event %d has unexpected error: %v", i, evt.Err)
		}
		if string(evt.Data) != want[i] {
			t.Errorf("event %d data = %q, want %q", i, evt.Data, want[i])
		}
	}
}

// TestDoNDJSONRequest_Non2xx proves a non-2xx status surfaces the status code
// and the response body in the returned error.
func TestDoNDJSONRequest_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := DoNDJSONRequest(context.Background(), srv.Client(), http.MethodPost, srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "model not found") {
		t.Errorf("error must carry status and body, got: %v", err)
	}
}

// TestDoNDJSONRequest_ContextCancel proves canceling the context terminates
// the stream (channel closes) without a spurious terminal error event.
func TestDoNDJSONRequest_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, "{\"a\":1}\n")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done() // block until the client goes away
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := DoNDJSONRequest(ctx, srv.Client(), http.MethodPost, srv.URL, nil, nil)
	if err != nil {
		t.Fatalf("DoNDJSONRequest: %v", err)
	}

	first, ok := <-ch
	if !ok || first.Err != nil || string(first.Data) != `{"a":1}` {
		t.Fatalf("first event = %+v (ok=%v)", first, ok)
	}

	cancel()

	for evt := range ch {
		if evt.Err != nil {
			t.Fatalf("cancel must not surface a terminal error event, got: %v", evt.Err)
		}
	}
}

// TestDoNDJSONRequest_IgnoresClientTimeout proves a long-lived stream is not
// truncated by the http.Client.Timeout (a whole-request deadline). Streaming
// lifetime must be governed by the context, not a fixed client timeout.
func TestDoNDJSONRequest_IgnoresClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fl, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "{\"n\":%d}\n", i)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(80 * time.Millisecond) // total ~240ms, exceeds client timeout below
		}
	}))
	defer srv.Close()

	client := srv.Client()
	client.Timeout = 100 * time.Millisecond // would truncate the stream after ~1 event

	ch, err := DoNDJSONRequest(context.Background(), client, http.MethodGet, srv.URL, nil, nil)
	if err != nil {
		t.Fatalf("DoNDJSONRequest: %v", err)
	}

	count := 0
	for range ch {
		count++
	}
	if count != 3 {
		t.Fatalf("got %d events, want 3 (client timeout truncated the stream)", count)
	}
}

// TestParseNDJSONStream_ScannerErrorTerminalEvent proves a scanner failure
// (here: a line exceeding the 1MB buffer cap) is surfaced as a terminal Err
// event instead of silently ending the stream.
func TestParseNDJSONStream_ScannerErrorTerminalEvent(t *testing.T) {
	pr, pw := io.Pipe()
	ch := make(chan NDJSONEvent, 4)

	go func() {
		_, _ = fmt.Fprint(pw, "{\"a\":1}\n")
		_, _ = fmt.Fprint(pw, strings.Repeat("x", 1024*1024+1), "\n")
		_ = pw.Close()
	}()

	go parseNDJSONStream(context.Background(), pr, ch)
	events := collectNDJSON(ch)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (data + terminal error): %+v", len(events), events)
	}
	if string(events[0].Data) != `{"a":1}` {
		t.Errorf("first event data = %q", events[0].Data)
	}
	if events[1].Err == nil {
		t.Fatal("scanner error was swallowed; expected terminal Err event")
	}
}

// TestParseNDJSONStream_ExitsOnCancelWhenSendBlocked proves the parser
// goroutine exits when the context is canceled even while parked on a channel
// send (which happens whenever the consumer has stopped draining).
func TestParseNDJSONStream_ExitsOnCancelWhenSendBlocked(t *testing.T) {
	pr, pw := io.Pipe()
	ch := make(chan NDJSONEvent) // unbuffered + no consumer => the first send blocks
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		parseNDJSONStream(ctx, pr, ch)
		close(done)
	}()

	// Feed one complete line so the parser reaches `ch <- event` and blocks
	// (nothing is draining ch).
	go func() { _, _ = fmt.Fprint(pw, "{\"a\":1}\n") }()
	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case <-done:
		// parser returned => no leak
	case <-time.After(2 * time.Second):
		t.Fatal("parseNDJSONStream leaked: did not exit on cancel while blocked on send")
	}
	_ = pw.Close()
}
