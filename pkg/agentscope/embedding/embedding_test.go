package embedding

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// --- splitBatches ---

func TestSplitBatches(t *testing.T) {
	tests := []struct {
		name      string
		texts     []string
		batchSize int
		want      int // number of batches
	}{
		{"empty", nil, 2, 1},
		{"single item", []string{"a"}, 2, 1},
		{"exact fit", []string{"a", "b"}, 2, 1},
		{"overflow by one", []string{"a", "b", "c"}, 2, 2},
		{"multiple full", []string{"a", "b", "c", "d"}, 2, 2},
		{"batch larger than input", []string{"a"}, 100, 1},
		{"batch size zero", []string{"a", "b"}, 0, 1},
		{"batch size negative", []string{"a", "b"}, -1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			batches := splitBatches(tt.texts, tt.batchSize)
			if len(batches) != tt.want {
				t.Errorf("got %d batches, want %d", len(batches), tt.want)
			}
			var total int
			for _, b := range batches {
				total += len(b)
			}
			if total != len(tt.texts) {
				t.Errorf("total items %d, want %d", total, len(tt.texts))
			}
		})
	}
}

// --- CacheKey ---

func TestCacheKey(t *testing.T) {
	k1 := CacheKey("model-a", 768, []string{"hello"})
	k2 := CacheKey("model-a", 768, []string{"hello"})
	k3 := CacheKey("model-a", 512, []string{"hello"})
	k4 := CacheKey("model-b", 768, []string{"hello"})

	if k1 != k2 {
		t.Error("same inputs should produce same key")
	}
	if k1 == k3 {
		t.Error("different dimensions should produce different key")
	}
	if k1 == k4 {
		t.Error("different model should produce different key")
	}
	if len(k1) != 64 {
		t.Errorf("expected 64-char hex string, got %d chars", len(k1))
	}
}

// --- AsEmbedder ---

type mockEmbeddingModel struct {
	name       string
	embeddings [][]float32
	err        error
}

func (m *mockEmbeddingModel) Embed(_ context.Context, texts []string) (*EmbeddingResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &EmbeddingResponse{
		Embeddings: m.embeddings[:len(texts)],
		Source:     "api",
	}, nil
}

func (m *mockEmbeddingModel) ModelName() string { return m.name }

func TestAsEmbedder(t *testing.T) {
	mock := &mockEmbeddingModel{
		name:       "test-model",
		embeddings: [][]float32{{0.1, 0.2}, {0.3, 0.4}},
	}

	embedder := AsEmbedder(mock)
	result, err := embedder.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 embeddings, got %d", len(result))
	}
	if result[0][0] != 0.1 || result[1][0] != 0.3 {
		t.Error("embedding values mismatch")
	}
}

func TestAsEmbedder_Error(t *testing.T) {
	mock := &mockEmbeddingModel{err: fmt.Errorf("api error")}
	embedder := AsEmbedder(mock)
	_, err := embedder.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Error("expected error")
	}
}

// --- FileEmbeddingCache ---

func TestFileEmbeddingCache_StoreRetrieve(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewFileEmbeddingCache(dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	key := "test-key-abc"
	data := [][]float32{{0.1, 0.2, 0.3}, {0.4, 0.5, 0.6}}

	if err := cache.Store(key, data); err != nil {
		t.Fatalf("Store: %v", err)
	}

	got, ok := cache.Retrieve(key)
	if !ok {
		t.Fatal("Retrieve returned false for stored key")
	}
	if len(got) != 2 || len(got[0]) != 3 {
		t.Fatalf("shape mismatch: got %d×%d", len(got), len(got[0]))
	}
	if got[0][0] != 0.1 || got[1][2] != 0.6 {
		t.Error("value mismatch")
	}
}

func TestFileEmbeddingCache_RetrieveMissing(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewFileEmbeddingCache(dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	_, ok := cache.Retrieve("nonexistent")
	if ok {
		t.Error("expected false for missing key")
	}
}

func TestFileEmbeddingCache_Remove(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewFileEmbeddingCache(dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	key := "to-remove"
	_ = cache.Store(key, [][]float32{{1.0}})

	if err := cache.Remove(key); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	_, ok := cache.Retrieve(key)
	if ok {
		t.Error("key should not exist after Remove")
	}
}

func TestFileEmbeddingCache_Clear(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewFileEmbeddingCache(dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	_ = cache.Store("k1", [][]float32{{1.0}})
	_ = cache.Store("k2", [][]float32{{2.0}})

	if err := cache.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	if _, ok := cache.Retrieve("k1"); ok {
		t.Error("k1 should not exist after Clear")
	}
	if _, ok := cache.Retrieve("k2"); ok {
		t.Error("k2 should not exist after Clear")
	}
}

func TestFileEmbeddingCache_EvictByCount(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewFileEmbeddingCache(dir, 2, 0)
	if err != nil {
		t.Fatal(err)
	}

	_ = cache.Store("a", [][]float32{{1.0}})
	_ = cache.Store("b", [][]float32{{2.0}})
	_ = cache.Store("c", [][]float32{{3.0}})

	entries, _ := os.ReadDir(dir)
	jsonCount := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			jsonCount++
		}
	}
	if jsonCount > 2 {
		t.Errorf("expected at most 2 files after eviction, got %d", jsonCount)
	}
}

// --- OpenAI-compatible model ---

func newOpenAIMockServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req openAIEmbeddingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		data := make([]openAIEmbeddingData, len(req.Input))
		for i := range req.Input {
			data[i] = openAIEmbeddingData{
				Object:    "embedding",
				Index:     i,
				Embedding: []float32{float32(i) * 0.1, float32(i)*0.1 + 0.01, float32(i)*0.1 + 0.02},
			}
		}

		resp := openAIEmbeddingResponse{
			Object: "list",
			Data:   data,
			Model:  req.Model,
			Usage:  &openAIEmbeddingUsage{PromptTokens: len(req.Input) * 3, TotalTokens: len(req.Input) * 3},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestOpenAICompatEmbed(t *testing.T) {
	server := newOpenAIMockServer(t)
	defer server.Close()

	model, err := NewOpenAIEmbeddingModel(&OpenAICompatConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
		Model:   "text-embedding-3-small",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := model.Embed(context.Background(), []string{"hello", "world"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(resp.Embeddings) != 2 {
		t.Fatalf("expected 2 embeddings, got %d", len(resp.Embeddings))
	}
	if resp.Source != "api" {
		t.Errorf("expected source=api, got %s", resp.Source)
	}
	if resp.Usage == nil || resp.Usage.Tokens != 6 {
		t.Errorf("unexpected usage: %+v", resp.Usage)
	}
	if resp.Embeddings[0][0] != 0.0 {
		t.Errorf("unexpected first embedding value: %f", resp.Embeddings[0][0])
	}
}

func TestOpenAICompatEmbed_Empty(t *testing.T) {
	model, err := NewOpenAIEmbeddingModel(&OpenAICompatConfig{
		APIKey:  "test-key",
		BaseURL: "http://unused",
		Model:   "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := model.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Embeddings) != 0 {
		t.Errorf("expected 0 embeddings for empty input, got %d", len(resp.Embeddings))
	}
}

func TestOpenAICompatEmbed_Batching(t *testing.T) {
	var callCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		var req openAIEmbeddingRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		data := make([]openAIEmbeddingData, len(req.Input))
		for i := range req.Input {
			data[i] = openAIEmbeddingData{
				Index:     i,
				Embedding: []float32{float32(n), float32(i)},
			}
		}

		resp := openAIEmbeddingResponse{
			Data:  data,
			Usage: &openAIEmbeddingUsage{TotalTokens: len(req.Input)},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	model, err := newOpenAICompat(&OpenAICompatConfig{
		APIKey:    "test",
		BaseURL:   server.URL,
		Model:     "test",
		BatchSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := model.Embed(context.Background(), []string{"a", "b", "c", "d", "e"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(resp.Embeddings) != 5 {
		t.Fatalf("expected 5 embeddings, got %d", len(resp.Embeddings))
	}
	if callCount.Load() < 3 {
		t.Errorf("expected at least 3 API calls for batch_size=2, got %d", callCount.Load())
	}
	if resp.Usage == nil || resp.Usage.Tokens != 5 {
		t.Errorf("expected total tokens=5, got %+v", resp.Usage)
	}
}

func TestOpenAICompatEmbed_SortsByIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openAIEmbeddingResponse{
			Data: []openAIEmbeddingData{
				{Index: 2, Embedding: []float32{2.0}},
				{Index: 0, Embedding: []float32{0.0}},
				{Index: 1, Embedding: []float32{1.0}},
			},
			Usage: &openAIEmbeddingUsage{TotalTokens: 3},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	model, _ := newOpenAICompat(&OpenAICompatConfig{
		APIKey:  "test",
		BaseURL: server.URL,
		Model:   "test",
	})

	resp, err := model.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}

	for i, emb := range resp.Embeddings {
		if emb[0] != float32(i) {
			t.Errorf("embedding[%d] = %f, want %f (index sorting failed)", i, emb[0], float32(i))
		}
	}
}

func TestOpenAICompatEmbed_DenseEmbeddingFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openAIEmbeddingResponse{
			Data: []openAIEmbeddingData{
				{Index: 0, DenseEmbedding: []float32{9.9}},
			},
			Usage: &openAIEmbeddingUsage{TotalTokens: 1},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	model, _ := newOpenAICompat(&OpenAICompatConfig{
		APIKey:  "test",
		BaseURL: server.URL,
		Model:   "test",
	})

	resp, err := model.Embed(context.Background(), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Embeddings[0][0] != 9.9 {
		t.Errorf("expected dense_embedding fallback, got %f", resp.Embeddings[0][0])
	}
}

func TestOpenAICompatEmbed_WithCache(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := openAIEmbeddingResponse{
			Data:  []openAIEmbeddingData{{Index: 0, Embedding: []float32{1.0}}},
			Usage: &openAIEmbeddingUsage{TotalTokens: 1},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cache, _ := NewFileEmbeddingCache(t.TempDir(), 0, 0)
	model, _ := newOpenAICompat(&OpenAICompatConfig{
		APIKey:  "test",
		BaseURL: server.URL,
		Model:   "test",
		Cache:   cache,
	})

	// First call: API
	resp1, err := model.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatal(err)
	}
	if resp1.Source != "api" {
		t.Errorf("first call: expected source=api, got %s", resp1.Source)
	}
	if callCount != 1 {
		t.Errorf("first call: expected 1 API call, got %d", callCount)
	}

	// Second call: cache
	resp2, err := model.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatal(err)
	}
	if resp2.Source != "cache" {
		t.Errorf("second call: expected source=cache, got %s", resp2.Source)
	}
	if callCount != 1 {
		t.Errorf("second call: expected no additional API calls, got %d", callCount)
	}
	if resp2.Embeddings[0][0] != 1.0 {
		t.Errorf("cached embedding value mismatch")
	}
}

func TestOpenAICompatEmbed_ModelName(t *testing.T) {
	model, _ := newOpenAICompat(&OpenAICompatConfig{
		BaseURL: "http://test",
		Model:   "my-model",
	})
	if model.ModelName() != "my-model" {
		t.Errorf("expected my-model, got %s", model.ModelName())
	}
}

func TestOpenAICompatEmbed_Dimensions(t *testing.T) {
	server := newOpenAIMockServer(t)
	defer server.Close()

	model, _ := NewDashScopeEmbeddingModel(&OpenAICompatConfig{
		APIKey:     "test",
		BaseURL:    server.URL,
		Model:      "text-embedding-v3",
		Dimensions: 512,
	})
	if model.Dimensions() != 512 {
		t.Errorf("expected 512, got %d", model.Dimensions())
	}
}

// --- Constructor validation ---

func TestNewOpenAI_RequiresKey(t *testing.T) {
	_, err := NewOpenAIEmbeddingModel(&OpenAICompatConfig{Model: "test"})
	if err == nil {
		t.Error("expected error for missing API key")
	}
}

func TestNewDashScope_RequiresKey(t *testing.T) {
	_, err := NewDashScopeEmbeddingModel(&OpenAICompatConfig{Model: "test"})
	if err == nil {
		t.Error("expected error for missing API key")
	}
}

func TestNewOllama_NoKeyRequired(t *testing.T) {
	_, err := NewOllamaEmbeddingModel(&OpenAICompatConfig{Model: "test"})
	if err != nil {
		t.Errorf("Ollama should not require API key: %v", err)
	}
}

func TestNewOpenAICompat_RequiresModel(t *testing.T) {
	_, err := newOpenAICompat(&OpenAICompatConfig{BaseURL: "http://test"})
	if err == nil {
		t.Error("expected error for missing model")
	}
}

// --- Gemini model ---

func newGeminiMockServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req geminiBatchEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		embeddings := make([]geminiEmbeddingResult, len(req.Requests))
		for i := range req.Requests {
			embeddings[i] = geminiEmbeddingResult{
				Values: []float32{float32(i) * 0.5, float32(i)*0.5 + 0.01},
			}
		}

		resp := geminiBatchEmbedResponse{Embeddings: embeddings}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestGeminiEmbed(t *testing.T) {
	server := newGeminiMockServer(t)
	defer server.Close()

	model, err := NewGeminiEmbeddingModel(&GeminiConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
		Model:   "gemini-embedding-001",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := model.Embed(context.Background(), []string{"hello", "world", "test"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(resp.Embeddings) != 3 {
		t.Fatalf("expected 3 embeddings, got %d", len(resp.Embeddings))
	}
	if resp.Source != "api" {
		t.Errorf("expected source=api, got %s", resp.Source)
	}
	if resp.Embeddings[1][0] != 0.5 {
		t.Errorf("unexpected value: %f", resp.Embeddings[1][0])
	}
}

func TestGeminiEmbed_Empty(t *testing.T) {
	model, _ := NewGeminiEmbeddingModel(&GeminiConfig{
		APIKey:  "test",
		BaseURL: "http://unused",
		Model:   "test",
	})

	resp, err := model.Embed(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Embeddings) != 0 {
		t.Errorf("expected 0 embeddings, got %d", len(resp.Embeddings))
	}
}

func TestGeminiEmbed_WithCache(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := geminiBatchEmbedResponse{
			Embeddings: []geminiEmbeddingResult{{Values: []float32{7.7}}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cache, _ := NewFileEmbeddingCache(t.TempDir(), 0, 0)
	model, _ := NewGeminiEmbeddingModel(&GeminiConfig{
		APIKey:  "test",
		BaseURL: server.URL,
		Model:   "test",
		Cache:   cache,
	})

	resp1, _ := model.Embed(context.Background(), []string{"x"})
	if resp1.Source != "api" || callCount != 1 {
		t.Error("first call should hit API")
	}

	resp2, _ := model.Embed(context.Background(), []string{"x"})
	if resp2.Source != "cache" || callCount != 1 {
		t.Error("second call should hit cache")
	}
}

func TestNewGemini_RequiresKey(t *testing.T) {
	_, err := NewGeminiEmbeddingModel(&GeminiConfig{Model: "test"})
	if err == nil {
		t.Error("expected error for missing API key")
	}
}

func TestNewGemini_RequiresModel(t *testing.T) {
	_, err := NewGeminiEmbeddingModel(&GeminiConfig{APIKey: "test"})
	if err == nil {
		t.Error("expected error for missing model")
	}
}

func TestGeminiEmbed_ModelName(t *testing.T) {
	model, _ := NewGeminiEmbeddingModel(&GeminiConfig{
		APIKey: "test",
		Model:  "gemini-embedding-2",
	})
	if model.ModelName() != "gemini-embedding-2" {
		t.Errorf("expected gemini-embedding-2, got %s", model.ModelName())
	}
}

// --- batchEmbed ---

func TestBatchEmbed_Empty(t *testing.T) {
	called := false
	fn := func(_ context.Context, _ []string) (*EmbeddingResponse, error) {
		called = true
		return nil, nil
	}

	resp, err := batchEmbed(context.Background(), nil, 10, fn)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("fn should not be called for empty input")
	}
	if resp.Source != "api" {
		t.Errorf("expected source=api, got %s", resp.Source)
	}
}

func TestBatchEmbed_SingleBatch(t *testing.T) {
	fn := func(_ context.Context, texts []string) (*EmbeddingResponse, error) {
		emb := make([][]float32, len(texts))
		for i := range texts {
			emb[i] = []float32{float32(i)}
		}
		return &EmbeddingResponse{
			Embeddings: emb,
			Usage:      &EmbeddingUsage{Tokens: len(texts)},
			Source:     "api",
		}, nil
	}

	resp, err := batchEmbed(context.Background(), []string{"a", "b"}, 10, fn)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Embeddings) != 2 {
		t.Fatalf("expected 2 embeddings, got %d", len(resp.Embeddings))
	}
}

func TestBatchEmbed_MultiBatch(t *testing.T) {
	var callCount atomic.Int32
	fn := func(_ context.Context, texts []string) (*EmbeddingResponse, error) {
		n := callCount.Add(1)
		emb := make([][]float32, len(texts))
		for i := range texts {
			emb[i] = []float32{float32(n*100 + int32(i))}
		}
		return &EmbeddingResponse{
			Embeddings: emb,
			Usage:      &EmbeddingUsage{Tokens: len(texts)},
			Source:     "api",
		}, nil
	}

	resp, err := batchEmbed(context.Background(), []string{"a", "b", "c"}, 2, fn)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Embeddings) != 3 {
		t.Fatalf("expected 3 embeddings, got %d", len(resp.Embeddings))
	}
	if resp.Usage.Tokens != 3 {
		t.Errorf("expected 3 tokens, got %d", resp.Usage.Tokens)
	}
}

func TestBatchEmbed_Error(t *testing.T) {
	fn := func(_ context.Context, texts []string) (*EmbeddingResponse, error) {
		return nil, fmt.Errorf("api failure")
	}

	_, err := batchEmbed(context.Background(), []string{"a", "b", "c"}, 2, fn)
	if err == nil {
		t.Error("expected error")
	}
}
