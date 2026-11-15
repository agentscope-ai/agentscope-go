package memory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Fixtures mirror the response shapes captured from ReMe v0.4.1.13 (official
// Docker image). Unknown fields (link_expansion, counts, auto_tag, ...) are
// kept on purpose: parsing must tolerate them.

const reMeSearchHitJSON = `{"answer":"...","success":true,"metadata":{"results":[{"id":"d6dee6bf","text":"team decided to migrate to microservices","metadata":{},"path":"daily/2026-10-14/team-decisions.md","start_line":7,"end_line":12,"scores":{"keyword":1.798,"score":1.798}}],"link_expansion":{},"counts":{"vector":0,"keyword":1,"returned":1,"hybrid":false}}}`

const reMeSearchEmptyJSON = `{"answer":"","success":true,"metadata":{"results":[],"link_expansion":{},"counts":{"vector":0,"keyword":0,"returned":0,"hybrid":false}}}`

const reMeSearchErrorJSON = `{"answer":"Error: query cannot be empty","success":false,"metadata":{}}`

const reMeAutoMemoryCreatedJSON = `{"answer":"I did the note.","success":true,"metadata":{"date":"2026-10-14","path":"daily/2026-10-14/note.md","created":true,"modified":false,"n_messages":1,"auto_tag":{"processed":0,"succeeded":0,"failed":0,"ignored":[],"results":[]}}}`

const reMeAutoMemoryNotCreatedJSON = `{"answer":"I did the note.","success":true,"metadata":{"date":"2026-10-14","path":null,"created":false,"modified":false,"n_messages":2,"auto_tag":{"processed":0,"succeeded":0,"failed":0,"ignored":[],"results":[]}}}`

const reMeAutoMemoryErrorJSON = `{"answer":"Error: session_id is required","success":false,"metadata":{"auto_tag":{"processed":0,"succeeded":0,"failed":0,"ignored":[],"results":[]}}}`

func newTestReMeStore(t *testing.T, serverURL string, filterBySession bool) *ReMeStore {
	t.Helper()
	store, err := NewReMeStore(ReMeConfig{BaseURL: serverURL, FilterBySession: filterBySession})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func reMeFixtureServer(t *testing.T, wantPath string, captured *map[string]any, response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != wantPath {
			t.Errorf("expected path %s, got %s", wantPath, r.URL.Path)
		}
		if captured != nil {
			if err := json.NewDecoder(r.Body).Decode(captured); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
}

func TestReMeStore_Defaults(t *testing.T) {
	store, err := NewReMeStore(ReMeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if store.baseURL != defaultReMeBaseURL {
		t.Errorf("baseURL = %q, want %q", store.baseURL, defaultReMeBaseURL)
	}
	if store.searchTimeout != defaultReMeSearchTimeout || store.addTimeout != defaultReMeAddTimeout {
		t.Errorf("timeouts = %v/%v, want %v/%v", store.searchTimeout, store.addTimeout,
			defaultReMeSearchTimeout, defaultReMeAddTimeout)
	}
}

func TestReMeStore_NegativeTimeout(t *testing.T) {
	if _, err := NewReMeStore(ReMeConfig{SearchTimeout: -time.Second}); err == nil {
		t.Error("expected error for negative SearchTimeout")
	}
	if _, err := NewReMeStore(ReMeConfig{AddTimeout: -time.Second}); err == nil {
		t.Error("expected error for negative AddTimeout")
	}
}

func TestReMeStore_ImplementsInterface(t *testing.T) {
	var _ MemoryStore = (*ReMeStore)(nil)
}

// TestReMeStore_Search_WireFormat asserts the full request body: query, limit
// from TopK, min_score from Threshold, and search_filter with the combined
// session_id when FilterBySession is on.
func TestReMeStore_Search_WireFormat(t *testing.T) {
	var captured map[string]any
	server := reMeFixtureServer(t, "/search", &captured, reMeSearchEmptyJSON)
	defer server.Close()

	store := newTestReMeStore(t, server.URL, true)
	_, err := store.Search(context.Background(), "coffee", "user1", &SearchOptions{TopK: 3, Threshold: 0.5, AgentID: "agent1"})
	if err != nil {
		t.Fatal(err)
	}

	if captured["query"] != "coffee" {
		t.Errorf("query = %v", captured["query"])
	}
	if captured["limit"] != float64(3) {
		t.Errorf("limit = %v, want 3", captured["limit"])
	}
	if captured["min_score"] != 0.5 {
		t.Errorf("min_score = %v, want 0.5", captured["min_score"])
	}
	filter, ok := captured["search_filter"].(map[string]any)
	if !ok || filter["session_id"] != "user1:agent1" {
		t.Errorf("search_filter = %v, want session_id user1:agent1", captured["search_filter"])
	}
}

// TestReMeStore_Search_WireFormat_Minimal proves limit/min_score stay off the
// wire when unset (server defaults apply) and search_filter is not sent when
// FilterBySession is off.
func TestReMeStore_Search_WireFormat_Minimal(t *testing.T) {
	var captured map[string]any
	server := reMeFixtureServer(t, "/search", &captured, reMeSearchEmptyJSON)
	defer server.Close()

	store := newTestReMeStore(t, server.URL, false)
	if _, err := store.Search(context.Background(), "coffee", "user1", nil); err != nil {
		t.Fatal(err)
	}

	if len(captured) != 1 || captured["query"] != "coffee" {
		t.Errorf("body = %v, want only the query field", captured)
	}
}

func TestReMeStore_Search_ParsesHit(t *testing.T) {
	server := reMeFixtureServer(t, "/search", nil, reMeSearchHitJSON)
	defer server.Close()

	store := newTestReMeStore(t, server.URL, false)
	results, err := store.Search(context.Background(), "microservices", "user1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].ID != "d6dee6bf" {
		t.Errorf("ID = %q", results[0].ID)
	}
	if results[0].Text != "team decided to migrate to microservices" {
		t.Errorf("Text = %q", results[0].Text)
	}
	// ReMe results carry neither the user nor a creation timestamp.
	if results[0].UserID != "" || !results[0].CreatedAt.IsZero() {
		t.Errorf("UserID/CreatedAt must stay empty/zero, got %+v", results[0])
	}
}

func TestReMeStore_Search_Empty(t *testing.T) {
	server := reMeFixtureServer(t, "/search", nil, reMeSearchEmptyJSON)
	defer server.Close()

	store := newTestReMeStore(t, server.URL, false)
	results, err := store.Search(context.Background(), "nothing", "user1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

// TestReMeStore_Search_SuccessFalse proves a business error arrives as
// HTTP 200 with success=false and must be surfaced from the envelope, not the
// status code.
func TestReMeStore_Search_SuccessFalse(t *testing.T) {
	server := reMeFixtureServer(t, "/search", nil, reMeSearchErrorJSON)
	defer server.Close()

	store := newTestReMeStore(t, server.URL, false)
	_, err := store.Search(context.Background(), "", "user1", nil)
	if err == nil {
		t.Fatal("expected error for success=false envelope")
	}
	if !strings.Contains(err.Error(), "query cannot be empty") {
		t.Errorf("error should carry the answer text, got: %v", err)
	}
	if errors.Is(err, ErrStoreUnsupported) {
		t.Error("business error must not match ErrStoreUnsupported")
	}
}

// TestReMeStore_Search_MissingMetadata proves a success envelope without
// metadata parses as an empty result.
func TestReMeStore_Search_MissingMetadata(t *testing.T) {
	server := reMeFixtureServer(t, "/search", nil, `{"answer":"","success":true}`)
	defer server.Close()

	store := newTestReMeStore(t, server.URL, false)
	results, err := store.Search(context.Background(), "x", "user1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestReMeStore_Search_MalformedMetadata(t *testing.T) {
	server := reMeFixtureServer(t, "/search", nil, `{"answer":"","success":true,"metadata":{"results":"not-a-list"}}`)
	defer server.Close()

	store := newTestReMeStore(t, server.URL, false)
	if _, err := store.Search(context.Background(), "x", "user1", nil); err == nil {
		t.Fatal("expected error for malformed results")
	}
}

// TestReMeStore_Search_SuccessFalseNoAnswer covers the envelope error with an
// empty answer field.
func TestReMeStore_Search_SuccessFalseNoAnswer(t *testing.T) {
	server := reMeFixtureServer(t, "/search", nil, `{"answer":"","success":false,"metadata":{}}`)
	defer server.Close()

	store := newTestReMeStore(t, server.URL, false)
	_, err := store.Search(context.Background(), "x", "user1", nil)
	if err == nil {
		t.Fatal("expected error for success=false envelope")
	}
	if !strings.Contains(err.Error(), "no details") {
		t.Errorf("error should note the missing details, got: %v", err)
	}
}

// TestReMeStore_Add_MalformedMetadata proves an unparseable metadata payload
// on a successful add is tolerated (it is informational only).
func TestReMeStore_Add_MalformedMetadata(t *testing.T) {
	server := reMeFixtureServer(t, "/auto_memory", nil, `{"answer":"ok","success":true,"metadata":"junk"}`)
	defer server.Close()

	store := newTestReMeStore(t, server.URL, false)
	if err := store.Add(context.Background(), "text", "user1", ""); err != nil {
		t.Fatalf("malformed metadata must not fail the add, got: %v", err)
	}
}

func TestReMeStore_Search_HTTP500(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	store := newTestReMeStore(t, server.URL, false)
	_, err := store.Search(context.Background(), "x", "user1", nil)
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should carry the status, got: %v", err)
	}
}

// TestReMeStore_Add_WireFormat asserts the auto_memory request: the message
// carries name/role/content, and session_id combines userID and agentID.
func TestReMeStore_Add_WireFormat(t *testing.T) {
	for _, tc := range []struct {
		name        string
		agentID     string
		wantSession string
	}{
		{"with_agent", "agent1", "user1:agent1"},
		{"user_only", "", "user1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured map[string]any
			server := reMeFixtureServer(t, "/auto_memory", &captured, reMeAutoMemoryCreatedJSON)
			defer server.Close()

			store := newTestReMeStore(t, server.URL, false)
			if err := store.Add(context.Background(), "I like coffee", "user1", tc.agentID); err != nil {
				t.Fatal(err)
			}

			if captured["session_id"] != tc.wantSession {
				t.Errorf("session_id = %v, want %q", captured["session_id"], tc.wantSession)
			}
			msgs, ok := captured["messages"].([]any)
			if !ok || len(msgs) != 1 {
				t.Fatalf("messages = %v", captured["messages"])
			}
			msg, _ := msgs[0].(map[string]any)
			if msg["name"] != "user1" || msg["role"] != "user" || msg["content"] != "I like coffee" {
				t.Errorf("message = %v", msg)
			}
		})
	}
}

// TestReMeStore_Add_NotCreated proves success with metadata.created=false is
// not an error: the server-side LLM decided the content was not worth storing.
func TestReMeStore_Add_NotCreated(t *testing.T) {
	server := reMeFixtureServer(t, "/auto_memory", nil, reMeAutoMemoryNotCreatedJSON)
	defer server.Close()

	store := newTestReMeStore(t, server.URL, false)
	if err := store.Add(context.Background(), "hi", "user1", ""); err != nil {
		t.Fatalf("created=false must not be an error, got: %v", err)
	}
}

func TestReMeStore_Add_SuccessFalse(t *testing.T) {
	server := reMeFixtureServer(t, "/auto_memory", nil, reMeAutoMemoryErrorJSON)
	defer server.Close()

	store := newTestReMeStore(t, server.URL, false)
	err := store.Add(context.Background(), "text", "user1", "")
	if err == nil {
		t.Fatal("expected error for success=false envelope")
	}
	if !strings.Contains(err.Error(), "session_id is required") {
		t.Errorf("error should carry the answer text, got: %v", err)
	}
}

func TestReMeStore_Add_TransportError(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	store := newTestReMeStore(t, deadURL, false)
	if err := store.Add(context.Background(), "text", "user1", ""); err == nil {
		t.Fatal("expected transport error")
	}
}

func TestReMeStore_Add_EmptyText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not make API call for empty text")
	}))
	defer server.Close()

	store := newTestReMeStore(t, server.URL, false)
	if err := store.Add(context.Background(), "", "user1", ""); err != nil {
		t.Fatal(err)
	}
}

// TestReMeStore_ListDelete_Unsupported proves List/Delete report the
// ErrStoreUnsupported sentinel, while a transport failure does not match it.
func TestReMeStore_ListDelete_Unsupported(t *testing.T) {
	store := newTestReMeStore(t, "http://localhost:2333", false)

	if _, err := store.List(context.Background(), "user1"); !errors.Is(err, ErrStoreUnsupported) {
		t.Errorf("List error = %v, want errors.Is ErrStoreUnsupported", err)
	}
	if err := store.Delete(context.Background(), "id1"); !errors.Is(err, ErrStoreUnsupported) {
		t.Errorf("Delete error = %v, want errors.Is ErrStoreUnsupported", err)
	}

	// A closed server gives a connection-refused transport error, which must
	// NOT match the sentinel.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	deadStore := newTestReMeStore(t, deadURL, false)
	_, err := deadStore.Search(context.Background(), "x", "user1", nil)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if errors.Is(err, ErrStoreUnsupported) {
		t.Error("transport error must not match ErrStoreUnsupported")
	}
}

func TestReMeStore_Search_ContextCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	store := newTestReMeStore(t, server.URL, false)

	errCh := make(chan error, 1)
	go func() {
		_, err := store.Search(ctx, "x", "user1", nil)
		errCh <- err
	}()
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Search did not return after context cancel")
	}
}
