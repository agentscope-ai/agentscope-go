package httpx

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"
)

// TestParseSSEStream_ExitsOnCancelWhenSendBlocked proves the SSE parser goroutine
// exits when the context is canceled even while parked on a channel send (which
// happens whenever the consumer has stopped draining). Before the fix the parser
// blocked forever on `ch <- event`, leaking the goroutine and holding the
// response body/socket open on every canceled or abandoned stream.
func TestParseSSEStream_ExitsOnCancelWhenSendBlocked(t *testing.T) {
	pr, pw := io.Pipe()
	ch := make(chan SSEEvent) // unbuffered + no consumer => the first send blocks
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		parseSSEStream(ctx, pr, ch)
		close(done)
	}()

	// Feed one complete event so the parser reaches `ch <- event` and blocks
	// (nothing is draining ch).
	go func() { _, _ = fmt.Fprint(pw, "data: hello\n\n") }()
	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case <-done:
		// parser returned => no leak
	case <-time.After(2 * time.Second):
		t.Fatal("parseSSEStream leaked: did not exit on cancel while blocked on send")
	}
	_ = pw.Close()
}
