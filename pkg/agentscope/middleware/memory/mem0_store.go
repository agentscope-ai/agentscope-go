package memory

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

const defaultMem0BaseURL = "https://api.mem0.ai/v1"

// Mem0Config holds the configuration for connecting to the mem0 REST API.
type Mem0Config struct {
	APIKey       string          // Required. API key for authentication.
	SecretAPIKey model.SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	BaseURL      string          // Base URL of the mem0 API. Default: https://api.mem0.ai/v1
	HTTPClient   *http.Client    // Optional custom HTTP client.
}

// Mem0Store implements MemoryStore by calling the mem0 REST API.
type Mem0Store struct {
	apiKey  string
	baseURL string
	client  *http.Client
	hdrs    map[string]string
}

// NewMem0Store creates a new mem0-backed memory store.
func NewMem0Store(cfg Mem0Config) (*Mem0Store, error) {
	apiKey := model.ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("mem0: API key is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultMem0BaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Mem0Store{
		apiKey:  apiKey,
		baseURL: base,
		client:  client,
		hdrs: map[string]string{
			"Authorization": "Token " + apiKey,
			"Content-Type":  "application/json",
		},
	}, nil
}

func (s *Mem0Store) headers() map[string]string {
	return s.hdrs
}

// mem0AddRequest is the request body for POST /memories.
type mem0AddRequest struct {
	Messages []mem0Message `json:"messages"`
	UserID   string        `json:"user_id"`
	AgentID  string        `json:"agent_id,omitempty"`
	Infer    *bool         `json:"infer,omitempty"`
}

type mem0Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// mem0AddResponse is the response from POST /memories.
type mem0AddResponse struct {
	Results []mem0Result `json:"results"`
}

type mem0Result struct {
	ID      string `json:"id"`
	Memory  string `json:"memory"`
	Event   string `json:"event"`
	UserID  string `json:"user_id"`
	AgentID string `json:"agent_id"`
}

// Add stores a memory via the mem0 API. If mem0 cannot extract any memories
// from the text (zero results), it retries with infer=false to store the raw text.
func (s *Mem0Store) Add(ctx context.Context, text string, userID string, agentID string) error {
	if text == "" {
		return nil
	}

	req := mem0AddRequest{
		Messages: []mem0Message{{Role: "user", Content: text}},
		UserID:   userID,
		AgentID:  agentID,
	}

	var resp mem0AddResponse
	url := s.baseURL + "/memories"
	if err := httpx.DoJSONRequest(ctx, s.client, http.MethodPost, url, req, &resp, s.headers()); err != nil {
		return fmt.Errorf("mem0: add memory: %w", err)
	}

	// If mem0 could not extract any memories, retry with infer=false to store raw text.
	if len(resp.Results) == 0 {
		inferFalse := false
		req.Infer = &inferFalse

		var retryResp mem0AddResponse
		if err := httpx.DoJSONRequest(ctx, s.client, http.MethodPost, url, req, &retryResp, s.headers()); err != nil {
			return fmt.Errorf("mem0: add memory (infer=false retry): %w", err)
		}
	}

	return nil
}

// mem0SearchRequest is the request body for POST /search.
type mem0SearchRequest struct {
	Query     string  `json:"query"`
	UserID    string  `json:"user_id"`
	AgentID   string  `json:"agent_id,omitempty"`
	TopK      int     `json:"top_k"`
	Threshold float64 `json:"threshold"`
}

// mem0SearchResponse is the response from POST /search.
type mem0SearchResponse struct {
	Results []mem0SearchResult `json:"results"`
}

type mem0SearchResult struct {
	ID        string  `json:"id"`
	Memory    string  `json:"memory"`
	UserID    string  `json:"user_id"`
	AgentID   string  `json:"agent_id"`
	Score     float64 `json:"score"`
	CreatedAt string  `json:"created_at"`
}

// Search queries mem0 for relevant memories.
func (s *Mem0Store) Search(ctx context.Context, query string, userID string, opts *SearchOptions) ([]Memory, error) {
	topK := 5
	threshold := 0.0
	agentID := ""
	if opts != nil {
		if opts.TopK > 0 {
			topK = opts.TopK
		}
		threshold = opts.Threshold
		agentID = opts.AgentID
	}

	req := mem0SearchRequest{
		Query:     query,
		UserID:    userID,
		AgentID:   agentID,
		TopK:      topK,
		Threshold: threshold,
	}

	var resp mem0SearchResponse
	url := s.baseURL + "/search"
	if err := httpx.DoJSONRequest(ctx, s.client, http.MethodPost, url, req, &resp, s.headers()); err != nil {
		return nil, fmt.Errorf("mem0: search: %w", err)
	}

	memories := make([]Memory, 0, len(resp.Results))
	for _, r := range resp.Results {
		mem := Memory{
			ID:      r.ID,
			Text:    r.Memory,
			UserID:  r.UserID,
			AgentID: r.AgentID,
		}
		if r.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, r.CreatedAt); err == nil {
				mem.CreatedAt = t
			}
		}
		memories = append(memories, mem)
	}
	return memories, nil
}

// mem0ListEntry is a single entry in the GET /memories response.
type mem0ListEntry struct {
	ID        string `json:"id"`
	Memory    string `json:"memory"`
	UserID    string `json:"user_id"`
	AgentID   string `json:"agent_id"`
	CreatedAt string `json:"created_at"`
}

// List retrieves all memories for a user from mem0.
func (s *Mem0Store) List(ctx context.Context, userID string) ([]Memory, error) {
	listURL := fmt.Sprintf("%s/memories?user_id=%s", s.baseURL, url.QueryEscape(userID))

	var entries []mem0ListEntry
	if err := httpx.DoJSONRequest(ctx, s.client, http.MethodGet, listURL, nil, &entries, s.headers()); err != nil {
		return nil, fmt.Errorf("mem0: list memories: %w", err)
	}

	memories := make([]Memory, 0, len(entries))
	for _, e := range entries {
		mem := Memory{
			ID:      e.ID,
			Text:    e.Memory,
			UserID:  e.UserID,
			AgentID: e.AgentID,
		}
		if e.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, e.CreatedAt); err == nil {
				mem.CreatedAt = t
			}
		}
		memories = append(memories, mem)
	}
	return memories, nil
}

// Delete removes a memory by ID from mem0.
func (s *Mem0Store) Delete(ctx context.Context, id string) error {
	url := fmt.Sprintf("%s/memories/%s", s.baseURL, id)
	if err := httpx.DoJSONRequest(ctx, s.client, http.MethodDelete, url, nil, nil, s.headers()); err != nil {
		return fmt.Errorf("mem0: delete memory %q: %w", id, err)
	}
	return nil
}
