package httpx

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDoSSERequest_IgnoresClientTimeout proves a long-lived stream is not
// truncated by the http.Client.Timeout (a whole-request deadline). Streaming
// lifetime must be governed by the context, not a fixed client timeout.
func TestDoSSERequest_IgnoresClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: e%d\n\n", i)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(80 * time.Millisecond) // total ~240ms, exceeds client timeout below
		}
	}))
	defer srv.Close()

	client := srv.Client()
	client.Timeout = 100 * time.Millisecond // would truncate the stream after ~1 event

	ch, err := DoSSERequest(context.Background(), client, http.MethodGet, srv.URL, nil, nil)
	if err != nil {
		t.Fatalf("DoSSERequest: %v", err)
	}

	count := 0
	for range ch {
		count++
	}
	if count != 3 {
		t.Fatalf("got %d events, want 3 (client timeout truncated the stream)", count)
	}
}
