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
var _ Index = (*MilvusIndex)(nil)

// MilvusConfig configures a Milvus vector store backend via the RESTful API v2.
type MilvusConfig struct {
	Address        string // e.g. "localhost:19530"
	CollectionName string
	Dims           int
	MetricType     string // default "COSINE"
	Token          string // authentication token
}

// MilvusIndex implements the Index interface using the Milvus RESTful API v2.
type MilvusIndex struct {
	cfg      MilvusConfig
	embedder Embedder
	client   *http.Client
}

// NewMilvusIndex constructs a Milvus-backed Index.
func NewMilvusIndex(cfg MilvusConfig, embedder Embedder) (*MilvusIndex, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("milvus: address is required")
	}
	if cfg.CollectionName == "" {
		return nil, fmt.Errorf("milvus: collection name is required")
	}
	if cfg.Dims <= 0 {
		return nil, fmt.Errorf("milvus: dims must be > 0")
	}
	if embedder == nil {
		return nil, fmt.Errorf("milvus: embedder is required")
	}
	if cfg.MetricType == "" {
		cfg.MetricType = "COSINE"
	}
	return &MilvusIndex{
		cfg:      cfg,
		embedder: embedder,
		client:   &http.Client{},
	}, nil
}

// baseURL constructs the Milvus REST API base URL.
func (m *MilvusIndex) baseURL() string {
	addr := strings.TrimRight(m.cfg.Address, "/")
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	return addr + "/v2/vectordb"
}

// doRequest sends a POST request to the Milvus REST API.
func (m *MilvusIndex) doRequest(ctx context.Context, path string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("milvus: marshal payload: %w", err)
	}

	url := m.baseURL() + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("milvus: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if m.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+m.cfg.Token)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("milvus: request %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("milvus: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("milvus: %s failed (%d): %s", path, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// ensureCollection creates the collection if it does not exist.
func (m *MilvusIndex) ensureCollection(ctx context.Context) error {
	payload := map[string]any{
		"collectionName": m.cfg.CollectionName,
		"dimension":      m.cfg.Dims,
		"metricType":     m.cfg.MetricType,
	}
	// The create collection endpoint is idempotent in Milvus v2 REST API.
	_, err := m.doRequest(ctx, "/collections/create", payload)
	if err != nil {
		// Ignore "already exists" errors.
		if strings.Contains(err.Error(), "already exist") {
			return nil
		}
		return fmt.Errorf("milvus: ensure collection: %w", err)
	}
	return nil
}

// AddDocuments embeds documents and inserts them into the Milvus collection.
func (m *MilvusIndex) AddDocuments(ctx context.Context, docs []Document) error {
	if len(docs) == 0 {
		return nil
	}

	if err := m.ensureCollection(ctx); err != nil {
		return err
	}

	// Embed all documents.
	texts := make([]string, len(docs))
	for i, d := range docs {
		texts[i] = d.Content
	}
	vectors, err := m.embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("milvus: embed documents: %w", err)
	}

	// Build data rows for insertion.
	data := make([]map[string]any, len(docs))
	for i, d := range docs {
		row := map[string]any{
			"id":      d.ID,
			"vector":  vectors[i],
			"content": d.Content,
		}
		// Flatten meta fields into the row for dynamic schema support.
		if d.Meta != nil {
			metaJSON, _ := json.Marshal(d.Meta)
			row["meta"] = string(metaJSON)
		}
		data[i] = row
	}

	payload := map[string]any{
		"collectionName": m.cfg.CollectionName,
		"data":           data,
	}

	respBody, err := m.doRequest(ctx, "/entities/insert", payload)
	if err != nil {
		return fmt.Errorf("milvus: insert: %w", err)
	}

	var insertResp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &insertResp); err == nil && insertResp.Code != 0 {
		return fmt.Errorf("milvus: insert error (code %d): %s", insertResp.Code, insertResp.Message)
	}
	return nil
}

// Query embeds the query string and searches for the top-K nearest vectors.
// normalizeScore maps the raw Milvus "distance" onto the project-wide
// higher-is-more-relevant direction (upstream #2486).
func (m *MilvusIndex) normalizeScore(distance float64) float64 {
	if strings.EqualFold(strings.TrimSpace(m.cfg.MetricType), "L2") {
		return -distance
	}
	return distance
}

func (m *MilvusIndex) Query(ctx context.Context, query string, topK int) ([]Document, error) {
	if topK <= 0 {
		topK = 10
	}

	vectors, err := m.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("milvus: embed query: %w", err)
	}
	queryVec := vectors[0]

	payload := map[string]any{
		"collectionName": m.cfg.CollectionName,
		"vector":         queryVec,
		"topK":           topK,
		"outputFields":   []string{"id", "content", "meta"},
	}

	respBody, err := m.doRequest(ctx, "/entities/search", payload)
	if err != nil {
		return nil, fmt.Errorf("milvus: search: %w", err)
	}

	var searchResp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    []struct {
			ID       string  `json:"id"`
			Content  string  `json:"content"`
			Meta     string  `json:"meta"`
			Distance float64 `json:"distance"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &searchResp); err != nil {
		return nil, fmt.Errorf("milvus: decode search response: %w", err)
	}
	if searchResp.Code != 0 {
		return nil, fmt.Errorf("milvus: search error (code %d): %s", searchResp.Code, searchResp.Message)
	}

	docs := make([]Document, 0, len(searchResp.Data))
	for _, hit := range searchResp.Data {
		d := Document{
			ID:      hit.ID,
			Content: hit.Content,
			// Upstream #2486: normalize so higher means more relevant.
			// Milvus L2 reports a distance (lower = closer) and is negated;
			// IP and COSINE report similarity-like scores already.
			Score: m.normalizeScore(hit.Distance),
		}
		if hit.Meta != "" {
			var meta map[string]any
			if err := json.Unmarshal([]byte(hit.Meta), &meta); err == nil {
				d.Meta = meta
			}
		}
		docs = append(docs, d)
	}
	return docs, nil
}
