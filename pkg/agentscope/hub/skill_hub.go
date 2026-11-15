package hub

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// SkillHubConfig holds configuration for a skill hub instance.
type SkillHubConfig struct {
	BaseURL      string          `json:"base_url"`
	APIKey       string          `json:"api_key,omitempty"`
	SecretAPIKey model.SecretStr `json:"secret_api_key,omitempty"` // Preferred over APIKey. Use model.NewSecretStr(key).
	HubID        string          `json:"hub_id"`
	DisplayName  string          `json:"display_name"`
}

// SkillHub implements Hub for skill registries.
type SkillHub struct {
	cfg    SkillHubConfig
	client *http.Client
}

// NewSkillHub creates a new skill hub client.
func NewSkillHub(cfg SkillHubConfig) *SkillHub { //nolint:gocritic // stable API: value receiver for backward compat
	return &SkillHub{
		cfg:    cfg,
		client: &http.Client{},
	}
}

func (h *SkillHub) ID() string          { return h.cfg.HubID }
func (h *SkillHub) DisplayName() string { return h.cfg.DisplayName }

func (h *SkillHub) List(ctx context.Context, opts *ListOptions) (*ListResult, error) {
	u, err := url.Parse(h.cfg.BaseURL + "/api/v1/skills")
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

func (h *SkillHub) Get(ctx context.Context, cardID string) (*Card, error) {
	u := h.cfg.BaseURL + "/api/v1/skills/" + url.PathEscape(cardID)

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

func (h *SkillHub) Install(ctx context.Context, cardID string, targetDir string) error {
	u := h.cfg.BaseURL + "/api/v1/skills/" + url.PathEscape(cardID) + "/install"

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

	destDir := filepath.Join(targetDir, cardID)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("hub: create target dir: %w", err)
	}

	if err := extractTarGz(resp.Body, destDir); err != nil {
		return fmt.Errorf("hub: extract archive: %w", err)
	}
	return nil
}

func (h *SkillHub) Close() error {
	h.client.CloseIdleConnections()
	return nil
}

func (h *SkillHub) setAuth(req *http.Request) {
	apiKey := model.ResolveAPIKey(h.cfg.APIKey, h.cfg.SecretAPIKey)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

// extractTarGz extracts a gzipped tar archive from r into destDir.
func extractTarGz(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag == tar.TypeDir && (hdr.Name == "." || hdr.Name == "./") {
			continue
		}

		// Prevent path traversal attacks.
		target := filepath.Join(destDir, hdr.Name)
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("invalid tar entry path: %s", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir parent: %w", err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o755)
			if err != nil {
				return fmt.Errorf("create file: %w", err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return fmt.Errorf("write file: %w", err)
			}
			_ = f.Close()
		}
	}
	return nil
}
