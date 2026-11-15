package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler_ServesIndexHTML(t *testing.T) {
	h := Handler(Options{})
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "AgentScope Studio") {
		t.Error("index.html should contain 'AgentScope Studio'")
	}
}

func TestHandler_ServesCSS(t *testing.T) {
	h := Handler(Options{})
	req := httptest.NewRequest("GET", "/style.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "--bg-primary") {
		t.Error("style.css should contain CSS variables")
	}
}

func TestHandler_ServesJS(t *testing.T) {
	h := Handler(Options{})
	req := httptest.NewRequest("GET", "/app.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "AgentScope Studio") {
		t.Error("app.js should contain app code")
	}
}

func TestHandler_SPAFallback(t *testing.T) {
	h := Handler(Options{})
	// Unknown path should fall back to index.html
	req := httptest.NewRequest("GET", "/some/unknown/path", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for SPA fallback, got %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "AgentScope Studio") {
		t.Error("SPA fallback should serve index.html")
	}
}

func TestMuxMount(t *testing.T) {
	mux := http.NewServeMux()
	MuxMount(mux, Options{})

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "AgentScope Studio") {
		t.Error("MuxMount should serve the web UI at /")
	}
}

func TestMuxMount_WithPrefix(t *testing.T) {
	mux := http.NewServeMux()
	MuxMount(mux, Options{PathPrefix: "/ui"})

	req := httptest.NewRequest("GET", "/ui/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHandler_Options(t *testing.T) {
	h := Handler(Options{Title: "Custom Title"})
	if h == nil {
		t.Fatal("Handler should not be nil")
	}
}

func TestHandler_StaticPrefixCSS(t *testing.T) {
	h := Handler(Options{})
	req := httptest.NewRequest("GET", "/static/style.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "--bg-primary") {
		t.Errorf("/static/style.css should contain CSS variables, got:\n%s", string(body)[:200])
	}
}

func TestHandler_StaticPrefixJS(t *testing.T) {
	h := Handler(Options{})
	req := httptest.NewRequest("GET", "/static/app.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "AgentScope Studio") {
		t.Errorf("/static/app.js should contain app code, got:\n%s", string(body)[:200])
	}
}
