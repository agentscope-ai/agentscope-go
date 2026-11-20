package embedding

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestDashScopeMultimodalTextType(t *testing.T) {
	requests := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Parameters map[string]string
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests <- req.Parameters["text_type"]
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()

	cfg := DashScopeMultimodalConfig{APIKey: "test", Model: "qwen3-vl-embedding", BaseURL: server.URL}
	m, err := NewDashScopeMultimodalEmbeddingModel(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Embed(t.Context(), []string{"query"}); err != nil {
		t.Fatal(err)
	}
	cfg.TextType = "document"
	m, err = NewDashScopeMultimodalEmbeddingModel(cfg)
	if err != nil {
		t.Fatal(err)
	}
	inputs := make([]MultimodalInput, 21)
	for i := range inputs {
		inputs[i].Text = "text"
	}
	if _, err := m.EmbedMultimodalWithTextType(t.Context(), inputs, "query"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EmbedMultimodal(t.Context(), inputs[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EmbedMultimodalWithTextType(t.Context(), inputs[:1], "invalid"); err == nil {
		t.Fatal("expected invalid override to fail")
	}
	cfg.TextType = "invalid"
	if _, err := NewDashScopeMultimodalEmbeddingModel(cfg); err == nil {
		t.Fatal("expected invalid config to fail")
	}
	server.Close()
	close(requests)
	var got []string
	for value := range requests {
		got = append(got, value)
	}
	if want := []string{"query", "query", "query", "document"}; !slices.Equal(got, want) {
		t.Fatalf("text_type values = %v, want %v", got, want)
	}
}
