package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// MCPHubConfig holds configuration for an MCP hub instance.
type MCPHubConfig struct {
	BaseURL      string          `json:"base_url"`
	APIKey       string          `json:"api_key,omitempty"`
	SecretAPIKey model.SecretStr `json:"secret_api_key,omitempty"` // Preferred over APIKey. Use model.NewSecretStr(key).
	HubID        string          `json:"hub_id"`
	DisplayName  string          `json:"display_name"`
}

// MCPHub implements Hub for MCP server registries.
type MCPHub struct {
	cfg    MCPHubConfig
	client *http.Client
}

// NewMCPHub creates a new MCP hub client.
func NewMCPHub(cfg MCPHubConfig) *MCPHub { //nolint:gocritic // stable API: value receiver for backward compat
	return &MCPHub{
		cfg:    cfg,
		client: &http.Client{},
	}
}

func (h *MCPHub) ID() string          { return h.cfg.HubID }
func (h *MCPHub) DisplayName() string { return h.cfg.DisplayName }

func (h *MCPHub) List(ctx context.Context, opts *ListOptions) (*ListResult, error) {
	u, err := url.Parse(h.cfg.BaseURL + "/api/v1/mcp")
	if err != nil {
		return nil, fmt.Errorf("hub: parse base url: %w", err)
	}

	q := u.Query()
	if opts.Query != "" {
		q.Set("query", opts.Query)
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if opts.Owner != "" {
		q.Set("owner", opts.Owner)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	q.Set("limit", strconv.Itoa(limit))
	for _, tag := range opts.Tags {
		q.Add("tag", tag)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("hub: create request: %w", err)
	}
	h.setAuth(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("hub: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var result ListResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("hub: decode response: %w", err)
	}
	return &result, nil
}

func (h *MCPHub) Get(ctx context.Context, cardID string) (*Card, error) {
	u := h.cfg.BaseURL + "/api/v1/mcp/" + url.PathEscape(cardID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("hub: create request: %w", err)
	}
	h.setAuth(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("hub: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var card Card
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		return nil, fmt.Errorf("hub: decode response: %w", err)
	}
	return &card, nil
}

func (h *MCPHub) Install(ctx context.Context, cardID string, targetDir string) error {
	u := h.cfg.BaseURL + "/api/v1/mcp/" + url.PathEscape(cardID) + "/install"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("hub: create request: %w", err)
	}
	h.setAuth(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("hub: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("hub: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("hub: create target dir: %w", err)
	}

	outPath := filepath.Join(targetDir, cardID+".json")
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("hub: create file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("hub: write file: %w", err)
	}
	return nil
}

func (h *MCPHub) Close() error {
	h.client.CloseIdleConnections()
	return nil
}

func (h *MCPHub) setAuth(req *http.Request) {
	apiKey := model.ResolveAPIKey(h.cfg.APIKey, h.cfg.SecretAPIKey)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}
