package embedding_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/embedding"
)

type endpointFixture struct {
	calls atomic.Int32
	url   string
}

func newEmbeddingEndpoint(t *testing.T, family string, value int) *endpointFixture {
	t.Helper()
	fixture := new(endpointFixture)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/embeddings"
		if family == "gemini" {
			want = "/models/offline-model:batchEmbedContents"
		}
		if r.Method != http.MethodPost || r.URL.Path != want {
			t.Errorf("request = %s %s, want POST %s", r.Method, r.URL.Path, want)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fixture.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if family == "gemini" {
			_, _ = fmt.Fprintf(w, `{"embeddings":[{"values":[%d]}]}`, value)
		} else {
			_, _ = fmt.Fprintf(w, `{"data":[{"index":0,"embedding":[%d]}]}`, value)
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	fixture.url = server.URL
	return fixture
}

func newEndpointModel(t *testing.T, family, url string, cache embedding.EmbeddingCache) embedding.EmbeddingModel {
	t.Helper()
	var model embedding.EmbeddingModel
	var err error
	switch family {
	case "openai":
		model, err = embedding.NewOpenAIEmbeddingModel(&embedding.OpenAICompatConfig{
			APIKey: "offline-fixture", BaseURL: url, Model: "offline-model", Dimensions: 1, Cache: cache,
		})
	case "gemini":
		model, err = embedding.NewGeminiEmbeddingModel(&embedding.GeminiConfig{
			APIKey: "offline-fixture", BaseURL: url, Model: "offline-model", Dimensions: 1, Cache: cache,
		})
	default:
		t.Fatalf("unexpected family: %s", family)
	}
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func embedEndpoint(t *testing.T, model embedding.EmbeddingModel) (float32, string) {
	t.Helper()
	result, err := model.Embed(context.Background(), []string{"same input"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Embeddings) != 1 || len(result.Embeddings[0]) != 1 {
		t.Fatalf("invalid fixture response: %+v", result)
	}
	return result.Embeddings[0][0], result.Source
}

func TestEndpointIsolation(t *testing.T) {
	for _, family := range []string{"openai", "gemini"} {
		t.Run(family, func(t *testing.T) {
			left := newEmbeddingEndpoint(t, family, 1)
			right := newEmbeddingEndpoint(t, family, 2)
			dir := t.TempDir()
			firstCache, err := embedding.NewFileEmbeddingCache(dir, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			first := newEndpointModel(t, family, left.url, firstCache)
			if value, source := embedEndpoint(t, first); value != 1 || source != "api" {
				t.Fatalf("first endpoint = %v (%s), want 1 (api)", value, source)
			}
			reopenedCache, err := embedding.NewFileEmbeddingCache(dir, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			sameEndpoint := newEndpointModel(t, family, left.url, reopenedCache)
			if value, source := embedEndpoint(t, sameEndpoint); value != 1 || source != "cache" || left.calls.Load() != 1 {
				t.Fatalf("same endpoint control = %v (%s), HTTP calls %d, want 1 (cache), one HTTP call", value, source, left.calls.Load())
			}
			uncached := newEndpointModel(t, family, right.url, nil)
			if value, source := embedEndpoint(t, uncached); value != 2 || source != "api" || right.calls.Load() != 1 {
				t.Fatalf("second endpoint uncached control = %v (%s), HTTP calls %d, want 2 (api), one HTTP call", value, source, right.calls.Load())
			}
			t.Logf("controls passed: cache reopened on first endpoint, and second endpoint returned its own vector without cache")
			second := newEndpointModel(t, family, right.url, reopenedCache)
			value, source := embedEndpoint(t, second)
			t.Logf("second endpoint with shared cache: vector=%v source=%s second endpoint total HTTP calls=%d", value, source, right.calls.Load())
			if value != 2 || source != "api" || right.calls.Load() != 2 {
				t.Fatalf("endpoint isolation violated: got %v (%s) with %d second-endpoint requests, want 2 (api) with 2 requests", value, source, right.calls.Load())
			}
		})
	}
}

func TestEndpointCacheScopesRequestShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/v1/embeddings":
			var body struct {
				Dimensions int
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode dimensions: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			value := 1
			if body.Dimensions == 0 {
				value = 4
			}
			_, _ = fmt.Fprintf(w, `{"data":[{"index":0,"embedding":[%d]}]}`, value)
		case "/v1//embeddings":
			_, _ = fmt.Fprint(w, `{"data":[{"index":0,"embedding":[2]}]}`)
		case "/v1/models/offline-model:batchEmbedContents":
			_, _ = fmt.Fprint(w, `{"embeddings":[{"values":[3]}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.EscapedPath())
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	cache, err := embedding.NewFileEmbeddingCache(t.TempDir(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	legacyKey := embedding.CacheKey("offline-model", 1, []string{"same input"})
	if err := cache.Store(legacyKey, [][]float32{{99}}); err != nil {
		t.Fatal(err)
	}

	baseURL := server.URL + "/v1"
	omitted, err := embedding.NewOpenAIEmbeddingModel(&embedding.OpenAICompatConfig{
		APIKey: "offline-fixture", BaseURL: baseURL, Model: "offline-model",
		Dimensions: 1, OmitDimensions: true, Cache: cache,
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		model embedding.EmbeddingModel
		value float32
	}{
		{"old key misses", newEndpointModel(t, "openai", baseURL, cache), 1},
		{"distinct request path", newEndpointModel(t, "openai", baseURL+"/", cache), 2},
		{"distinct adapter family", newEndpointModel(t, "gemini", baseURL, cache), 3},
		{"dimensions omitted on wire", omitted, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			value, source := embedEndpoint(t, tc.model)
			if value != tc.value || source != "api" {
				t.Fatalf("first result = %v (%s), want %v (api)", value, source, tc.value)
			}
			value, source = embedEndpoint(t, tc.model)
			if value != tc.value || source != "cache" {
				t.Fatalf("second result = %v (%s), want %v (cache)", value, source, tc.value)
			}
		})
	}
}
