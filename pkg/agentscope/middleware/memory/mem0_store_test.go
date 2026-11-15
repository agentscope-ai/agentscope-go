package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// newTestMem0Store creates a Mem0Store pointed at the given test server.
func newTestMem0Store(t *testing.T, serverURL string) *Mem0Store {
	t.Helper()
	store, err := NewMem0Store(Mem0Config{
		APIKey:  "test-api-key",
		BaseURL: serverURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestMem0Store_NewRequiresAPIKey(t *testing.T) {
	_, err := NewMem0Store(Mem0Config{})
	if err == nil {
		t.Error("expected error when APIKey is empty")
	}
}

func TestMem0Store_DefaultBaseURL(t *testing.T) {
	store, err := NewMem0Store(Mem0Config{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if store.baseURL != defaultMem0BaseURL {
		t.Errorf("expected default base URL %q, got %q", defaultMem0BaseURL, store.baseURL)
	}
}

func TestMem0Store_Add_Success(t *testing.T) {
	var mu sync.Mutex
	var receivedBody mem0AddRequest
	callCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		callCount++

		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/memories") {
			t.Errorf("expected /memories path, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Token test-api-key" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}

		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		resp := mem0AddResponse{
			Results: []mem0Result{
				{ID: "mem_1", Memory: "likes coffee", Event: "ADD"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	store := newTestMem0Store(t, server.URL)
	err := store.Add(context.Background(), "I like coffee", "user1", "agent1")
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()

	if callCount != 1 {
		t.Errorf("expected 1 API call, got %d", callCount)
	}
	if len(receivedBody.Messages) != 1 || receivedBody.Messages[0].Content != "I like coffee" {
		t.Error("request body mismatch")
	}
	if receivedBody.UserID != "user1" {
		t.Errorf("expected user_id=user1, got %s", receivedBody.UserID)
	}
	if receivedBody.AgentID != "agent1" {
		t.Errorf("expected agent_id=agent1, got %s", receivedBody.AgentID)
	}
}

func TestMem0Store_Add_EmptyText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not make API call for empty text")
	}))
	defer server.Close()

	store := newTestMem0Store(t, server.URL)
	if err := store.Add(context.Background(), "", "user1", ""); err != nil {
		t.Fatal(err)
	}
}

func TestMem0Store_Add_ZeroResults_RetryWithInferFalse(t *testing.T) {
	var mu sync.Mutex
	callCount := 0
	var secondBody mem0AddRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		call := callCount
		mu.Unlock()

		var body mem0AddRequest
		_ = json.NewDecoder(r.Body).Decode(&body)

		w.Header().Set("Content-Type", "application/json")

		if call == 1 {
			// First call: return zero results.
			_ = json.NewEncoder(w).Encode(mem0AddResponse{Results: []mem0Result{}})
		} else {
			// Second call: capture the retry body and return success.
			mu.Lock()
			secondBody = body
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(mem0AddResponse{
				Results: []mem0Result{{ID: "mem_1", Memory: "raw text", Event: "ADD"}},
			})
		}
	}))
	defer server.Close()

	store := newTestMem0Store(t, server.URL)
	err := store.Add(context.Background(), "some obscure text", "user1", "")
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()

	if callCount != 2 {
		t.Fatalf("expected 2 API calls (initial + retry), got %d", callCount)
	}
	if secondBody.Infer == nil || *secondBody.Infer != false {
		t.Error("retry should set infer=false")
	}
}

func TestMem0Store_Search(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/search") {
			t.Errorf("expected /search path, got %s", r.URL.Path)
		}

		var body mem0SearchRequest
		_ = json.NewDecoder(r.Body).Decode(&body)

		if body.Query != "coffee" {
			t.Errorf("expected query=coffee, got %s", body.Query)
		}
		if body.UserID != "user1" {
			t.Errorf("expected user_id=user1, got %s", body.UserID)
		}
		if body.TopK != 3 {
			t.Errorf("expected top_k=3, got %d", body.TopK)
		}

		resp := mem0SearchResponse{
			Results: []mem0SearchResult{
				{
					ID:        "mem_1",
					Memory:    "likes coffee in the morning",
					UserID:    "user1",
					AgentID:   "agent1",
					Score:     0.95,
					CreatedAt: "2024-01-15T10:30:00Z",
				},
				{
					ID:        "mem_2",
					Memory:    "prefers dark roast coffee",
					UserID:    "user1",
					AgentID:   "agent1",
					Score:     0.88,
					CreatedAt: "2024-01-16T14:00:00Z",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	store := newTestMem0Store(t, server.URL)
	results, err := store.Search(context.Background(), "coffee", "user1", &SearchOptions{TopK: 3})
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].ID != "mem_1" {
		t.Errorf("expected first result ID=mem_1, got %s", results[0].ID)
	}
	if results[0].Text != "likes coffee in the morning" {
		t.Errorf("unexpected text: %s", results[0].Text)
	}
	if results[0].UserID != "user1" {
		t.Errorf("expected user_id=user1, got %s", results[0].UserID)
	}
	if results[0].CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be parsed")
	}
}

func TestMem0Store_Search_DefaultOptions(t *testing.T) {
	var receivedBody mem0SearchRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mem0SearchResponse{Results: []mem0SearchResult{}})
	}))
	defer server.Close()

	store := newTestMem0Store(t, server.URL)
	_, err := store.Search(context.Background(), "test", "user1", nil)
	if err != nil {
		t.Fatal(err)
	}

	if receivedBody.TopK != 5 {
		t.Errorf("expected default top_k=5, got %d", receivedBody.TopK)
	}
	if receivedBody.Threshold != 0 {
		t.Errorf("expected default threshold=0, got %f", receivedBody.Threshold)
	}
}

func TestMem0Store_Search_WithAgentID(t *testing.T) {
	var receivedBody mem0SearchRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mem0SearchResponse{Results: []mem0SearchResult{}})
	}))
	defer server.Close()

	store := newTestMem0Store(t, server.URL)
	_, err := store.Search(context.Background(), "test", "user1", &SearchOptions{AgentID: "agent1"})
	if err != nil {
		t.Fatal(err)
	}

	if receivedBody.AgentID != "agent1" {
		t.Errorf("expected agent_id=agent1, got %s", receivedBody.AgentID)
	}
}

func TestMem0Store_List(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/memories") {
			t.Errorf("expected /memories path, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("user_id") != "user1" {
			t.Errorf("expected user_id=user1 query param, got %s", r.URL.Query().Get("user_id"))
		}

		entries := []mem0ListEntry{
			{
				ID:        "mem_1",
				Memory:    "likes coffee",
				UserID:    "user1",
				AgentID:   "agent1",
				CreatedAt: "2024-01-15T10:30:00Z",
			},
			{
				ID:        "mem_2",
				Memory:    "prefers dark mode",
				UserID:    "user1",
				AgentID:   "agent1",
				CreatedAt: "2024-01-16T14:00:00Z",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	}))
	defer server.Close()

	store := newTestMem0Store(t, server.URL)
	memories, err := store.List(context.Background(), "user1")
	if err != nil {
		t.Fatal(err)
	}

	if len(memories) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(memories))
	}
	if memories[0].ID != "mem_1" {
		t.Errorf("expected ID=mem_1, got %s", memories[0].ID)
	}
	if memories[0].Text != "likes coffee" {
		t.Errorf("unexpected text: %s", memories[0].Text)
	}
	if memories[1].Text != "prefers dark mode" {
		t.Errorf("unexpected text: %s", memories[1].Text)
	}
	if memories[0].CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be parsed")
	}
}

func TestMem0Store_List_Empty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]mem0ListEntry{})
	}))
	defer server.Close()

	store := newTestMem0Store(t, server.URL)
	memories, err := store.List(context.Background(), "user1")
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 0 {
		t.Errorf("expected 0 memories, got %d", len(memories))
	}
}

func TestMem0Store_Delete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/memories/mem_123") {
			t.Errorf("expected /memories/mem_123 path, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Token test-api-key" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message": "Memory deleted successfully!"}`))
	}))
	defer server.Close()

	store := newTestMem0Store(t, server.URL)
	err := store.Delete(context.Background(), "mem_123")
	if err != nil {
		t.Fatal(err)
	}
}

func TestMem0Store_Add_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail": "Invalid API key"}`))
	}))
	defer server.Close()

	store := newTestMem0Store(t, server.URL)
	err := store.Add(context.Background(), "test", "user1", "")
	if err == nil {
		t.Error("expected error for 401 response")
	}
	if !strings.Contains(err.Error(), "mem0") {
		t.Errorf("error should mention mem0: %v", err)
	}
}

func TestMem0Store_Search_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail": "Invalid request"}`))
	}))
	defer server.Close()

	store := newTestMem0Store(t, server.URL)
	_, err := store.Search(context.Background(), "test", "user1", nil)
	if err == nil {
		t.Error("expected error for 400 response")
	}
}

func TestMem0Store_List_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail": "Internal server error"}`))
	}))
	defer server.Close()

	store := newTestMem0Store(t, server.URL)
	_, err := store.List(context.Background(), "user1")
	if err == nil {
		t.Error("expected error for 500 response")
	}
}

func TestMem0Store_Delete_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail": "Memory not found"}`))
	}))
	defer server.Close()

	store := newTestMem0Store(t, server.URL)
	err := store.Delete(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for 404 response")
	}
}

// TestMem0Store_ImplementsInterface verifies compile-time interface satisfaction.
func TestMem0Store_ImplementsInterface(t *testing.T) {
	var _ MemoryStore = (*Mem0Store)(nil)
}
