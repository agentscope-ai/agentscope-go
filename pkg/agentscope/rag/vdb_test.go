package rag

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeEmbedder returns zero vectors of dimension 3 for testing.
type fakeEmbedder struct{}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	vecs := make([][]float32, len(texts))
	for i := range vecs {
		vecs[i] = make([]float32, 3)
	}
	return vecs, nil
}

// --- Elasticsearch Tests ---

func TestElasticsearchIndex_AddDocuments(t *testing.T) {
	var headCalled, putCalled, bulkCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && strings.HasPrefix(r.URL.Path, "/test-index"):
			headCalled = true
			w.WriteHeader(http.StatusNotFound) // index does not exist yet
		case r.Method == http.MethodPut && r.URL.Path == "/test-index":
			putCalled = true
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"acknowledged":true}`))
		case r.Method == http.MethodPost && r.URL.Path == "/_bulk":
			bulkCalled = true
			body, _ := io.ReadAll(r.Body)
			// Verify NDJSON format: pairs of action + document lines
			lines := strings.Split(strings.TrimSpace(string(body)), "\n")
			if len(lines) != 4 { // 2 docs × 2 lines each
				t.Errorf("expected 4 NDJSON lines, got %d", len(lines))
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"errors":false,"items":[]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	idx, err := NewElasticsearchIndex(&ElasticsearchConfig{
		Addresses: []string{server.URL},
		IndexName: "test-index",
		Dims:      3,
	}, &fakeEmbedder{})
	if err != nil {
		t.Fatalf("NewElasticsearchIndex: %v", err)
	}

	docs := []Document{
		{ID: "doc1", Content: "first document", Meta: map[string]any{"k": "v1"}},
		{ID: "doc2", Content: "second document", Meta: map[string]any{"k": "v2"}},
	}
	if err := idx.AddDocuments(context.Background(), docs); err != nil {
		t.Fatalf("AddDocuments: %v", err)
	}
	if !headCalled {
		t.Error("expected HEAD request for index check")
	}
	if !putCalled {
		t.Error("expected PUT request for index creation")
	}
	if !bulkCalled {
		t.Error("expected POST /_bulk request")
	}
}

func TestElasticsearchIndex_Query(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_search") {
			resp := map[string]any{
				"hits": map[string]any{
					"hits": []map[string]any{
						{
							"_id": "doc1",
							"_source": map[string]any{
								"doc_id":  "doc1",
								"content": "hello world",
								"meta":    map[string]any{"source": "test"},
							},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	idx, err := NewElasticsearchIndex(&ElasticsearchConfig{
		Addresses: []string{server.URL},
		IndexName: "test-index",
		Dims:      3,
	}, &fakeEmbedder{})
	if err != nil {
		t.Fatalf("NewElasticsearchIndex: %v", err)
	}

	docs, err := idx.Query(context.Background(), "search term", 5)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 result, got %d", len(docs))
	}
	if docs[0].ID != "doc1" {
		t.Errorf("expected ID doc1, got %q", docs[0].ID)
	}
	if docs[0].Content != "hello world" {
		t.Errorf("expected content hello world, got %q", docs[0].Content)
	}
}

// --- MongoDB Tests ---

func TestMongoDBIndex_AddDocuments(t *testing.T) {
	var insertManyCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/action/insertMany") {
			insertManyCalled = true
			// Verify the request body structure
			var payload map[string]any
			json.NewDecoder(r.Body).Decode(&payload)
			if payload["database"] != "testdb" {
				t.Errorf("expected database testdb, got %v", payload["database"])
			}
			if payload["collection"] != "testcol" {
				t.Errorf("expected collection testcol, got %v", payload["collection"])
			}
			docs, ok := payload["documents"].([]any)
			if !ok || len(docs) != 2 {
				t.Errorf("expected 2 documents, got %v", payload["documents"])
			}
			resp := map[string]any{
				"insertedIds": []string{"doc1", "doc2"},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	idx, err := NewMongoDBIndex(&MongoDBConfig{
		BaseURL:    server.URL,
		Database:   "testdb",
		Collection: "testcol",
		IndexName:  "vector_index",
		Dims:       3,
		APIKey:     "test-key",
	}, &fakeEmbedder{})
	if err != nil {
		t.Fatalf("NewMongoDBIndex: %v", err)
	}

	docs := []Document{
		{ID: "doc1", Content: "first", Meta: map[string]any{"k": "v1"}},
		{ID: "doc2", Content: "second", Meta: map[string]any{"k": "v2"}},
	}
	if err := idx.AddDocuments(context.Background(), docs); err != nil {
		t.Fatalf("AddDocuments: %v", err)
	}
	if !insertManyCalled {
		t.Error("expected insertMany to be called")
	}
}

func TestMongoDBIndex_Query(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/action/aggregate") {
			resp := map[string]any{
				"documents": []map[string]any{
					{
						"_id":     "doc1",
						"content": "matched content",
						"meta":    map[string]any{"src": "test"},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	idx, err := NewMongoDBIndex(&MongoDBConfig{
		BaseURL:    server.URL,
		Database:   "testdb",
		Collection: "testcol",
		IndexName:  "vector_index",
		Dims:       3,
		APIKey:     "test-key",
	}, &fakeEmbedder{})
	if err != nil {
		t.Fatalf("NewMongoDBIndex: %v", err)
	}

	docs, err := idx.Query(context.Background(), "query text", 5)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 result, got %d", len(docs))
	}
	if docs[0].ID != "doc1" {
		t.Errorf("expected ID doc1, got %q", docs[0].ID)
	}
	if docs[0].Content != "matched content" {
		t.Errorf("expected content matched content, got %q", docs[0].Content)
	}
}

// --- Milvus Tests ---

func TestMilvusIndex_AddDocuments(t *testing.T) {
	var createCalled, insertCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/collections/create"):
			createCalled = true
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"code":0,"message":"success"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/entities/insert"):
			insertCalled = true
			var payload map[string]any
			json.NewDecoder(r.Body).Decode(&payload)
			if payload["collectionName"] != "test_collection" {
				t.Errorf("expected collectionName test_collection, got %v", payload["collectionName"])
			}
			data, ok := payload["data"].([]any)
			if !ok || len(data) != 2 {
				t.Errorf("expected 2 data rows, got %v", payload["data"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"code":0,"message":"success","data":{"insertIds":["doc1","doc2"]}}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	idx, err := NewMilvusIndex(MilvusConfig{
		Address:        server.URL,
		CollectionName: "test_collection",
		Dims:           3,
	}, &fakeEmbedder{})
	if err != nil {
		t.Fatalf("NewMilvusIndex: %v", err)
	}

	docs := []Document{
		{ID: "doc1", Content: "alpha", Meta: map[string]any{"k": "v1"}},
		{ID: "doc2", Content: "beta", Meta: map[string]any{"k": "v2"}},
	}
	if err := idx.AddDocuments(context.Background(), docs); err != nil {
		t.Fatalf("AddDocuments: %v", err)
	}
	if !createCalled {
		t.Error("expected collection create to be called")
	}
	if !insertCalled {
		t.Error("expected entities insert to be called")
	}
}

func TestMilvusIndex_Query(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/entities/search") {
			resp := map[string]any{
				"code":    0,
				"message": "success",
				"data": []map[string]any{
					{
						"id":      "doc1",
						"content": "found content",
						"meta":    `{"source":"milvus"}`,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	idx, err := NewMilvusIndex(MilvusConfig{
		Address:        server.URL,
		CollectionName: "test_collection",
		Dims:           3,
	}, &fakeEmbedder{})
	if err != nil {
		t.Fatalf("NewMilvusIndex: %v", err)
	}

	docs, err := idx.Query(context.Background(), "search query", 5)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 result, got %d", len(docs))
	}
	if docs[0].ID != "doc1" {
		t.Errorf("expected ID doc1, got %q", docs[0].ID)
	}
	if docs[0].Content != "found content" {
		t.Errorf("expected content found content, got %q", docs[0].Content)
	}
	if docs[0].Meta["source"] != "milvus" {
		t.Errorf("expected meta source milvus, got %v", docs[0].Meta["source"])
	}
}
