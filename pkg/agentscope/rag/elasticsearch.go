package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Compile-time interface check.
var _ Index = (*ElasticsearchIndex)(nil)

// ElasticsearchConfig configures an Elasticsearch vector store backend.
type ElasticsearchConfig struct {
	Addresses []string // e.g. ["http://localhost:9200"]
	IndexName string
	Username  string
	Password  string
	CloudID   string
	Dims      int
}

// ElasticsearchIndex implements the Index interface using Elasticsearch
// dense_vector fields and script_score queries with cosineSimilarity.
type ElasticsearchIndex struct {
	cfg      ElasticsearchConfig
	embedder Embedder
	client   *http.Client
}

// NewElasticsearchIndex constructs an Elasticsearch-backed Index.
func NewElasticsearchIndex(cfg *ElasticsearchConfig, embedder Embedder) (*ElasticsearchIndex, error) {
	if len(cfg.Addresses) == 0 {
		return nil, fmt.Errorf("elasticsearch: at least one address is required")
	}
	if cfg.IndexName == "" {
		return nil, fmt.Errorf("elasticsearch: index name is required")
	}
	if embedder == nil {
		return nil, fmt.Errorf("elasticsearch: embedder is required")
	}
	if cfg.Dims <= 0 {
		return nil, fmt.Errorf("elasticsearch: dims must be > 0")
	}
	return &ElasticsearchIndex{
		cfg:      *cfg,
		embedder: embedder,
		client:   &http.Client{},
	}, nil
}

// baseURL returns the first configured address (trimmed of trailing slash).
func (e *ElasticsearchIndex) baseURL() string {
	return strings.TrimRight(e.cfg.Addresses[0], "/")
}

// doRequest executes an HTTP request with optional basic auth.
func (e *ElasticsearchIndex) doRequest(req *http.Request) (*http.Response, error) {
	if e.cfg.Username != "" || e.cfg.Password != "" {
		req.SetBasicAuth(e.cfg.Username, e.cfg.Password)
	}
	req.Header.Set("Content-Type", "application/json")
	return e.client.Do(req)
}

// ensureIndex creates the index with dense_vector mapping if it does not exist.
func (e *ElasticsearchIndex) ensureIndex(ctx context.Context) error {
	// Check if index exists.
	url := fmt.Sprintf("%s/%s", e.baseURL(), e.cfg.IndexName)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return fmt.Errorf("elasticsearch: build HEAD request: %w", err)
	}
	resp, err := e.doRequest(req)
	if err != nil {
		return fmt.Errorf("elasticsearch: HEAD index: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil // already exists
	}

	// Create index with mapping.
	mapping := map[string]any{
		"mappings": map[string]any{
			"properties": map[string]any{
				"content": map[string]any{"type": "text"},
				"vector": map[string]any{
					"type": "dense_vector",
					"dims": e.cfg.Dims,
				},
				"meta":   map[string]any{"type": "object", "enabled": true},
				"doc_id": map[string]any{"type": "keyword"},
			},
		},
	}
	body, err := json.Marshal(mapping)
	if err != nil {
		return fmt.Errorf("elasticsearch: marshal mapping: %w", err)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("elasticsearch: build PUT request: %w", err)
	}
	resp, err = e.doRequest(req)
	if err != nil {
		return fmt.Errorf("elasticsearch: PUT index: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("elasticsearch: create index failed (%d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// AddDocuments creates the index if needed, then bulk indexes documents with embeddings.
func (e *ElasticsearchIndex) AddDocuments(ctx context.Context, docs []Document) error {
	if len(docs) == 0 {
		return nil
	}

	if err := e.ensureIndex(ctx); err != nil {
		return err
	}

	// Embed all documents.
	texts := make([]string, len(docs))
	for i, d := range docs {
		texts[i] = d.Content
	}
	vectors, err := e.embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("elasticsearch: embed documents: %w", err)
	}

	// Build bulk request body (NDJSON).
	var buf bytes.Buffer
	for i, d := range docs {
		action := map[string]any{
			"index": map[string]any{
				"_index": e.cfg.IndexName,
				"_id":    d.ID,
			},
		}
		actionLine, _ := json.Marshal(action)
		buf.Write(actionLine)
		buf.WriteByte('\n')

		doc := map[string]any{
			"doc_id":  d.ID,
			"content": d.Content,
			"vector":  vectors[i],
			"meta":    d.Meta,
		}
		docLine, _ := json.Marshal(doc)
		buf.Write(docLine)
		buf.WriteByte('\n')
	}

	url := fmt.Sprintf("%s/_bulk", e.baseURL())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return fmt.Errorf("elasticsearch: build bulk request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if e.cfg.Username != "" || e.cfg.Password != "" {
		req.SetBasicAuth(e.cfg.Username, e.cfg.Password)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("elasticsearch: bulk request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("elasticsearch: bulk failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var bulkResp struct {
		Errors bool `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&bulkResp); err == nil && bulkResp.Errors {
		return fmt.Errorf("elasticsearch: bulk indexing had errors")
	}
	return nil
}

// Query embeds the query string and performs a script_score search using cosineSimilarity.
func (e *ElasticsearchIndex) Query(ctx context.Context, query string, topK int) ([]Document, error) {
	if topK <= 0 {
		topK = 10
	}

	vectors, err := e.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("elasticsearch: embed query: %w", err)
	}
	queryVec := vectors[0]

	// Build script_score query with cosineSimilarity.
	searchBody := map[string]any{
		"size": topK,
		"query": map[string]any{
			"script_score": map[string]any{
				"query": map[string]any{"match_all": map[string]any{}},
				"script": map[string]any{
					"source": "cosineSimilarity(params.query_vector, 'vector') + 1.0",
					"params": map[string]any{
						"query_vector": queryVec,
					},
				},
			},
		},
	}
	body, err := json.Marshal(searchBody)
	if err != nil {
		return nil, fmt.Errorf("elasticsearch: marshal search body: %w", err)
	}

	url := fmt.Sprintf("%s/%s/_search", e.baseURL(), e.cfg.IndexName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("elasticsearch: build search request: %w", err)
	}
	resp, err := e.doRequest(req)
	if err != nil {
		return nil, fmt.Errorf("elasticsearch: search request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("elasticsearch: search failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Hits struct {
			Hits []struct {
				ID     string  `json:"_id"`
				Score  float64 `json:"_score"`
				Source struct {
					DocID   string         `json:"doc_id"`
					Content string         `json:"content"`
					Meta    map[string]any `json:"meta"`
				} `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("elasticsearch: decode search response: %w", err)
	}

	docs := make([]Document, 0, len(result.Hits.Hits))
	for _, hit := range result.Hits.Hits {
		docs = append(docs, Document{
			ID:      hit.Source.DocID,
			Content: hit.Source.Content,
			Meta:    hit.Source.Meta,
			// script_score is cosineSimilarity + 1.0: higher is already
			// more relevant (upstream #2486 normalization direction).
			Score: hit.Score,
		})
	}
	return docs, nil
}
