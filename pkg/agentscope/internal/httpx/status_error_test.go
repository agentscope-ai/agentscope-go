package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStatusErrorJSONAndSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"code":"blocked"}`))
	}))
	defer srv.Close()
	for _, stream := range []bool{false, true} {
		var err error
		if stream {
			_, err = DoSSERequest(context.Background(), srv.Client(), "POST", srv.URL, nil, nil)
		} else {
			err = DoJSONRequest(context.Background(), srv.Client(), "POST", srv.URL, nil, nil, nil)
		}
		var status *StatusError
		if !errors.As(err, &status) || status.StatusCode != 400 || string(status.Body) != `{"code":"blocked"}` || status.RetryAfter != 7*time.Second || err.Error() != `httpx: unexpected status 400: {"code":"blocked"}` {
			t.Fatalf("stream=%v: %#v", stream, err)
		}
	}
}
