package httpsec

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHarden_SetsTimeouts(t *testing.T) {
	srv := &http.Server{}
	Harden(srv)
	if srv.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout not set (Slowloris exposure)")
	}
	if srv.IdleTimeout == 0 {
		t.Error("IdleTimeout not set")
	}
	if srv.MaxHeaderBytes == 0 {
		t.Error("MaxHeaderBytes not set")
	}
}

func TestLimitBody_RejectsOversized(t *testing.T) {
	h := LimitBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := r.Body.Read(make([]byte, 1<<20))
		for err == nil {
			_, err = r.Body.Read(make([]byte, 1<<20))
		}
		if strings.Contains(err.Error(), "too large") {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}), 100)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 5000)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized body, got %d", rec.Code)
	}
}
