package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type testResp struct {
	OK bool `json:"ok"`
}

func TestDoJSONRequestSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_ = body
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(testResp{OK: true})
	}))
	defer srv.Close()

	var out testResp
	err := DoJSONRequest(
		context.Background(),
		nil,
		http.MethodPost,
		srv.URL,
		map[string]any{"foo": "bar"},
		&out,
		map[string]string{"X-Test": "1"},
	)
	if err != nil {
		t.Fatalf("DoJSONRequest failed: %v", err)
	}
	if !out.OK {
		t.Fatalf("unexpected response: %+v", out)
	}
}

// First call returns 500, second call returns 200; ensures retry-on-5xx works.
func TestDoJSONRequestRetryOn5xx(t *testing.T) {
	var called int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if called == 1 {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(testResp{OK: true})
	}))
	defer srv.Close()

	var out testResp
	err := DoJSONRequest(
		context.Background(),
		nil,
		http.MethodGet,
		srv.URL,
		nil,
		&out,
		nil,
	)
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if !out.OK {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestDoJSONRequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := DoJSONRequest(
		ctx,
		nil,
		http.MethodGet,
		srv.URL,
		nil,
		nil,
		nil,
	)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
}
