package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// Compile-time interface check.
var _ Index = (*MongoDBIndex)(nil)

// MongoDBConfig configures a MongoDB Atlas vector store backend via the Data API.
type MongoDBConfig struct {
	URI          string // unused for Data API, kept for future driver-based impl
	Database     string
	Collection   string
	IndexName    string // vector search index name
	Dims         int
	BaseURL      string          // Atlas Data API base URL (e.g. "https://data.mongodb-api.com/app/<appID>/endpoint/data/v1")
	APIKey       string          // Atlas Data API key
	SecretAPIKey model.SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
}

// MongoDBIndex implements the Index interface using MongoDB Atlas Data API
// with $vectorSearch aggregation pipeline.
type MongoDBIndex struct {
	cfg      MongoDBConfig
	embedder Embedder
	client   *http.Client
}

// NewMongoDBIndex constructs a MongoDB Atlas-backed Index.
func NewMongoDBIndex(cfg *MongoDBConfig, embedder Embedder) (*MongoDBIndex, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("mongodb: BaseURL is required")
	}
	if cfg.Database == "" {
		return nil, fmt.Errorf("mongodb: database is required")
	}
	if cfg.Collection == "" {
		return nil, fmt.Errorf("mongodb: collection is required")
	}
	if cfg.IndexName == "" {
		cfg.IndexName = "vector_index"
	}
	cfg.APIKey = model.ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("mongodb: APIKey is required")
	}
	if embedder == nil {
		return nil, fmt.Errorf("mongodb: embedder is required")
	}
	if cfg.Dims <= 0 {
		return nil, fmt.Errorf("mongodb: dims must be > 0")
	}
	return &MongoDBIndex{
		cfg:      *cfg,
		embedder: embedder,
		client:   &http.Client{},
	}, nil
}

// endpoint builds the full URL for a Data API action.
func (m *MongoDBIndex) endpoint(action string) string {
	base := strings.TrimRight(m.cfg.BaseURL, "/")
	return fmt.Sprintf("%s/action/%s", base, action)
}

// doRequest sends a POST request to the Atlas Data API.
func (m *MongoDBIndex) doRequest(ctx context.Context, action string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("mongodb: marshal payload: %w", err)
	}

	url := m.endpoint(action)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mongodb: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", m.cfg.APIKey)

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mongodb: %s request: %w", action, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mongodb: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mongodb: %s failed (%d): %s", action, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// AddDocuments inserts documents with vector embeddings via the insertMany action.
func (m *MongoDBIndex) AddDocuments(ctx context.Context, docs []Document) error {
	if len(docs) == 0 {
		return nil
	}

	// Embed all documents.
	texts := make([]string, len(docs))
	for i, d := range docs {
		texts[i] = d.Content
	}
	vectors, err := m.embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("mongodb: embed documents: %w", err)
	}

	// Build documents for insertion.
	mongoDocs := make([]map[string]any, len(docs))
	for i, d := range docs {
		mongoDocs[i] = map[string]any{
			"_id":       d.ID,
			"content":   d.Content,
			"embedding": vectors[i],
			"meta":      d.Meta,
		}
	}

	payload := map[string]any{
		"dataSource": "Cluster0",
		"database":   m.cfg.Database,
		"collection": m.cfg.Collection,
		"documents":  mongoDocs,
	}

	_, err = m.doRequest(ctx, "insertMany", payload)
	if err != nil {
		return fmt.Errorf("mongodb: insertMany: %w", err)
	}
	return nil
}

// Query embeds the query and uses $vectorSearch aggregation pipeline to find top-K documents.
func (m *MongoDBIndex) Query(ctx context.Context, query string, topK int) ([]Document, error) {
	if topK <= 0 {
		topK = 10
	}

	vectors, err := m.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("mongodb: embed query: %w", err)
	}
	queryVec := vectors[0]

	// Build $vectorSearch aggregation pipeline.
	pipeline := []map[string]any{
		{
			"$vectorSearch": map[string]any{
				"index":         m.cfg.IndexName,
				"path":          "embedding",
				"queryVector":   queryVec,
				"numCandidates": topK * 10,
				"limit":         topK,
			},
		},
		{
			"$project": map[string]any{
				"_id":     1,
				"content": 1,
				"meta":    1,
				"score":   map[string]any{"$meta": "vectorSearchScore"},
			},
		},
	}

	payload := map[string]any{
		"dataSource": "Cluster0",
		"database":   m.cfg.Database,
		"collection": m.cfg.Collection,
		"pipeline":   pipeline,
	}

	respBody, err := m.doRequest(ctx, "aggregate", payload)
	if err != nil {
		return nil, fmt.Errorf("mongodb: aggregate: %w", err)
	}

	var result struct {
		Documents []struct {
			ID      string         `json:"_id"`
			Content string         `json:"content"`
			Meta    map[string]any `json:"meta"`
			Score   float64        `json:"score"`
		} `json:"documents"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("mongodb: decode response: %w", err)
	}

	docs := make([]Document, 0, len(result.Documents))
	for _, d := range result.Documents {
		docs = append(docs, Document{
			ID:      d.ID,
			Content: d.Content,
			Meta:    d.Meta,
			// vectorSearchScore: higher is more relevant (upstream #2486).
			Score: d.Score,
		})
	}
	return docs, nil
}
