package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOpenAICompatEmbed_RejectsMismatchedCount(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inputs    []string
		returned  int
		wantError string
	}{
		{
			name:      "empty response for nonempty input",
			inputs:    []string{"one", "two"},
			returned:  0,
			wantError: "0 embeddings for 2 inputs",
		},
		{
			name:      "short batch",
			inputs:    []string{"one", "two"},
			returned:  1,
			wantError: "1 embeddings for 2 inputs",
		},
		{
			name:      "overlong batch",
			inputs:    []string{"one", "two"},
			returned:  3,
			wantError: "3 embeddings for 2 inputs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data := make([]openAIEmbeddingData, tc.returned)
				for i := range data {
					data[i] = openAIEmbeddingData{
						Index:     i,
						Embedding: []float32{float32(i + 1)},
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(openAIEmbeddingResponse{Data: data})
			}))
			defer srv.Close()

			cache, err := NewFileEmbeddingCache(t.TempDir(), 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			m, err := NewOllamaEmbeddingModel(&OpenAICompatConfig{
				BaseURL: srv.URL,
				Model:   "embed",
				Cache:   cache,
			})
			if err != nil {
				t.Fatal(err)
			}

			resp, err := m.Embed(context.Background(), tc.inputs)
			if err == nil {
				t.Fatalf("Embed succeeded with %d vectors for %d inputs", len(resp.Embeddings), len(tc.inputs))
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantError)
			}

			key := endpointCacheKey("openai-compatible", srv.URL, "embed", 0, tc.inputs)
			if _, ok := cache.Retrieve(key); ok {
				t.Fatal("mismatched response must not be stored in cache")
			}
		})
	}
}

func TestOpenAICompatEmbed_MismatchedBatchesCannotCancelOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openAIEmbeddingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var data []openAIEmbeddingData
		if len(req.Input) > 0 && req.Input[0] == "a" {
			data = []openAIEmbeddingData{{Index: 0, Embedding: []float32{1.0}}}
		} else {
			data = []openAIEmbeddingData{
				{Index: 0, Embedding: []float32{3.0}},
				{Index: 1, Embedding: []float32{4.0}},
				{Index: 2, Embedding: []float32{99.0}},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIEmbeddingResponse{Data: data})
	}))
	defer srv.Close()

	cache, err := NewFileEmbeddingCache(t.TempDir(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewOllamaEmbeddingModel(&OpenAICompatConfig{
		BaseURL:   srv.URL,
		Model:     "embed",
		BatchSize: 2,
		Cache:     cache,
	})
	if err != nil {
		t.Fatal(err)
	}

	inputs := []string{"a", "b", "c", "d"}
	if _, err := m.Embed(context.Background(), inputs); err == nil {
		t.Fatal("expected error when batch counts mismatch even though their sum equals len(inputs)")
	}
	key := endpointCacheKey("openai-compatible", srv.URL, "embed", 0, inputs)
	if _, ok := cache.Retrieve(key); ok {
		t.Fatal("failed multi-batch call must not write to cache")
	}
}

func TestOpenAICompatEmbed_InvalidCachedCountIsReplaced(t *testing.T) {
	for _, staleCount := range []int{0, 1, 3} {
		t.Run(string(rune('0'+staleCount)), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(openAIEmbeddingResponse{
					Data: []openAIEmbeddingData{
						{Index: 0, Embedding: []float32{1.0, 0.0}},
						{Index: 1, Embedding: []float32{0.0, 1.0}},
					},
				})
			}))
			defer srv.Close()

			cache, err := NewFileEmbeddingCache(t.TempDir(), 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			inputs := []string{"one", "two"}
			key := endpointCacheKey("openai-compatible", srv.URL, "embed", 0, inputs)
			stale := make([][]float32, staleCount)
			for i := range stale {
				stale[i] = []float32{9.0, 9.0}
			}
			if err := cache.Store(key, stale); err != nil {
				t.Fatal(err)
			}

			m, err := NewOllamaEmbeddingModel(&OpenAICompatConfig{
				BaseURL: srv.URL,
				Model:   "embed",
				Cache:   cache,
			})
			if err != nil {
				t.Fatal(err)
			}

			resp1, err := m.Embed(context.Background(), inputs)
			if err != nil {
				t.Fatalf("first Embed: %v", err)
			}
			if resp1.Source != "api" || len(resp1.Embeddings) != 2 || resp1.Embeddings[0][0] != 1.0 || resp1.Embeddings[1][1] != 1.0 {
				t.Fatalf("unexpected refetched response: %+v", resp1)
			}
			if calls.Load() != 1 {
				t.Fatalf("API calls = %d, want 1", calls.Load())
			}

			resp2, err := m.Embed(context.Background(), inputs)
			if err != nil {
				t.Fatalf("second Embed: %v", err)
			}
			if resp2.Source != "cache" || len(resp2.Embeddings) != 2 || resp2.Embeddings[0][0] != 1.0 || resp2.Embeddings[1][1] != 1.0 {
				t.Fatalf("unexpected cached response after replacement: %+v", resp2)
			}
			if calls.Load() != 1 {
				t.Fatalf("second call hit API (%d total calls), want cache hit", calls.Load())
			}
		})
	}
}

func TestOpenAICompatEmbed_OmittedIndexPreservesOrderLargeBatch(t *testing.T) {
	const n = 32
	inputs := make([]string, n)
	for i := range inputs {
		inputs[i] = string(rune('A' + i))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := make([]openAIEmbeddingData, n)
		for i := range data {
			data[i] = openAIEmbeddingData{
				Embedding: []float32{float32(i)},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIEmbeddingResponse{Data: data})
	}))
	defer srv.Close()

	m, err := NewOllamaEmbeddingModel(&OpenAICompatConfig{
		BaseURL: srv.URL,
		Model:   "embed",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := m.Embed(context.Background(), inputs)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for i, emb := range resp.Embeddings {
		if emb[0] != float32(i) {
			t.Fatalf("embedding[%d] = %v, want [%v]", i, emb, float32(i))
		}
	}
}
