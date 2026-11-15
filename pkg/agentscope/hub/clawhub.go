package hub

import (
	"archive/zip"
	"bytes"
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
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// ClawHubDefaultBaseURL is the public ClawHub skill registry.
const ClawHubDefaultBaseURL = "https://clawhub.ai"

// ClawHubConfig configures the built-in ClawHub skill source (port of
// Python's ClawSkillHub).
type ClawHubConfig struct {
	HubID       string          // default "clawhub"
	DisplayName string          // default "ClawHub"
	BaseURL     string          // default ClawHubDefaultBaseURL
	APIToken    model.SecretStr // optional, raises rate limits
	Timeout     time.Duration   // default 30s
}

// Compile-time interface check.
var _ Hub = (*ClawHub)(nil)

// ClawHub implements Hub over the ClawHub HTTP API. Card IDs are
// owner-scoped ("owner/slug") whenever the record names an owner,
// because a slug alone is not unique and the lookup endpoints answer
// 409 rather than guess (Python #2214 semantics).
type ClawHub struct {
	cfg    ClawHubConfig
	client *http.Client
}

// NewClawHub creates the hub with defaults applied.
func NewClawHub(cfg ClawHubConfig) *ClawHub {
	if cfg.HubID == "" {
		cfg.HubID = "clawhub"
	}
	if cfg.DisplayName == "" {
		cfg.DisplayName = "ClawHub"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = ClawHubDefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &ClawHub{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}
}

func (h *ClawHub) ID() string          { return h.cfg.HubID }
func (h *ClawHub) DisplayName() string { return h.cfg.DisplayName }

// List browses the catalog. Query switches to the search endpoint, which
// takes a query but returns a single page (no cursor); without Query the
// cursor-paginated catalog is used.
func (h *ClawHub) List(ctx context.Context, opts *ListOptions) (*ListResult, error) {
	if opts == nil {
		opts = &ListOptions{}
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	var records []map[string]any
	var nextCursor string

	if opts.Query != "" {
		u, err := url.Parse(h.cfg.BaseURL + "/api/v1/search")
		if err != nil {
			return nil, fmt.Errorf("hub: parse base url: %w", err)
		}
		q := u.Query()
		q.Set("q", opts.Query)
		q.Set("limit", strconv.Itoa(limit))
		u.RawQuery = q.Encode()
		var payload struct {
			Results []map[string]any `json:"results"`
		}
		if err := h.get(ctx, u.String(), &payload); err != nil {
			return nil, err
		}
		records = payload.Results
	} else {
		u, err := url.Parse(h.cfg.BaseURL + "/api/v1/skills")
		if err != nil {
			return nil, fmt.Errorf("hub: parse base url: %w", err)
		}
		q := u.Query()
		q.Set("limit", strconv.Itoa(limit))
		if opts.Cursor != "" {
			q.Set("cursor", opts.Cursor)
		}
		u.RawQuery = q.Encode()
		var payload struct {
			Items      []map[string]any `json:"items"`
			NextCursor string           `json:"nextCursor"`
		}
		if err := h.get(ctx, u.String(), &payload); err != nil {
			return nil, err
		}
		records = payload.Items
		nextCursor = payload.NextCursor
	}

	cards := make([]Card, 0, len(records))
	for _, item := range records {
		if card, ok := h.toCard(item); ok {
			cards = append(cards, card)
		}
	}
	return &ListResult{
		Cards:      cards,
		NextCursor: nextCursor,
		HasMore:    nextCursor != "",
	}, nil
}

// Get fetches one skill by card ID ("owner/slug" or bare slug).
func (h *ClawHub) Get(ctx context.Context, cardID string) (*Card, error) {
	slug, ownerHandle := splitClawCardID(cardID)
	u, err := url.Parse(h.cfg.BaseURL + "/api/v1/skills/" + url.PathEscape(slug))
	if err != nil {
		return nil, fmt.Errorf("hub: parse base url: %w", err)
	}
	if ownerHandle != "" {
		q := u.Query()
		q.Set("ownerHandle", ownerHandle)
		u.RawQuery = q.Encode()
	}
	var payload map[string]any
	if err := h.get(ctx, u.String(), &payload); err != nil {
		return nil, err
	}
	item, _ := payload["skill"].(map[string]any)
	if item == nil {
		item = payload
	}
	if _, ok := item["slug"]; !ok {
		item["slug"] = slug
	}
	if owner, ok := payload["owner"].(map[string]any); ok {
		item["owner"] = owner
	}
	card, ok := h.toCard(item)
	if !ok {
		return nil, fmt.Errorf("hub: clawhub returned no usable record for %q", cardID)
	}
	return &card, nil
}

// Install downloads the skill archive (GET /api/v1/download) and unpacks
// the ZIP into targetDir/<slug>/.
func (h *ClawHub) Install(ctx context.Context, cardID string, targetDir string) error {
	slug, ownerHandle := splitClawCardID(cardID)
	if !validSlug(slug) {
		return fmt.Errorf("hub: invalid skill slug %q", slug)
	}
	dest := filepath.Join(targetDir, slug)
	// Defense in depth: the slug charset check above already rules out
	// separators, so dest cannot escape targetDir — verify it anyway.
	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		return fmt.Errorf("hub: resolve target dir: %w", err)
	}
	absDest, err := filepath.Abs(dest)
	if err != nil || !strings.HasPrefix(absDest, absTarget+string(os.PathSeparator)) {
		return fmt.Errorf("hub: skill slug %q escapes target dir", slug)
	}
	createdDest := true
	u, err := url.Parse(h.cfg.BaseURL + "/api/v1/download")
	if err != nil {
		return fmt.Errorf("hub: parse base url: %w", err)
	}
	q := u.Query()
	q.Set("slug", slug)
	if ownerHandle != "" {
		q.Set("ownerHandle", ownerHandle)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("hub: create request: %w", err)
	}
	h.setAuth(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("hub: request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return fmt.Errorf("hub: read archive: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("hub: clawhub returned status %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	if _, err := os.Stat(dest); err == nil {
		createdDest = false
	}
	if err := unzip(bytes.NewReader(body), len(body), dest); err != nil {
		if createdDest {
			_ = os.RemoveAll(dest) // best effort: don't leave a half-unpacked skill behind
		}
		return fmt.Errorf("hub: unpack skill %q: %w", cardID, err)
	}
	return nil
}

func (h *ClawHub) Close() error {
	h.client.CloseIdleConnections()
	return nil
}

func (h *ClawHub) get(ctx context.Context, rawURL string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("hub: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	h.setAuth(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("hub: request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("hub: read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("hub: clawhub returned status %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("hub: decode response: %w", err)
	}
	return nil
}

func (h *ClawHub) setAuth(req *http.Request) {
	if token := h.cfg.APIToken.Value(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// splitClawCardID splits "owner/slug" into slug + owner handle; a bare
// slug yields an empty handle.
func splitClawCardID(cardID string) (slug, ownerHandle string) {
	if i := strings.LastIndex(cardID, "/"); i >= 0 {
		return cardID[i+1:], cardID[:i]
	}
	return cardID, ""
}

// toCard maps one catalog/search/detail record onto a Card.
func (h *ClawHub) toCard(item map[string]any) (Card, bool) {
	slug, _ := item["slug"].(string)
	if slug == "" {
		return Card{}, false
	}

	// Version: latestVersion object (catalog/detail) or plain version
	// (search).
	version := ""
	if latest, ok := item["latestVersion"].(map[string]any); ok {
		version, _ = latest["version"].(string)
	} else if v, ok := item["latestVersion"].(string); ok {
		version = v
	} else if v, ok := item["version"].(string); ok {
		version = v
	}

	owner, _ := item["owner"].(map[string]any)
	if owner == nil {
		if native, ok := item["native"].(map[string]any); ok {
			owner, _ = native["owner"].(map[string]any)
		}
	}
	handle := ""
	if v, ok := item["ownerHandle"].(string); ok && v != "" {
		handle = v
	} else if owner != nil {
		handle, _ = owner["handle"].(string)
	}

	desc, _ := item["summary"].(string)
	var tags []string
	if topics, ok := item["topics"].([]any); ok {
		for _, t := range topics {
			if s, ok := t.(string); ok {
				tags = append(tags, s)
			}
		}
	}

	card := Card{
		ID:          slug,
		Kind:        CardKindSkill,
		Name:        slug, // directory name — must stay the bare slug
		Description: desc,
		Version:     version,
		Tags:        tags,
	}
	if handle != "" {
		card.ID = handle + "/" + slug
		card.Owner = handle
	}
	if owner != nil {
		if img, ok := owner["image"].(string); ok {
			card.IconURL = img
		}
		if author, ok := owner["displayName"].(string); ok && author != "" {
			card.Meta = map[string]string{"author": author}
		}
	}
	if card.Meta == nil {
		card.Meta = map[string]string{}
	}
	card.Meta["url"] = h.cfg.BaseURL + "/skills/" + slug
	return card, true
}

// Extraction caps defending against zip bombs: one file may expand to at
// most maxUnzipFileSize bytes, and one archive to maxUnzipTotalSize bytes
// in aggregate.
const (
	maxUnzipFileSize  int64 = 64 << 20  // 64 MiB per extracted file
	maxUnzipTotalSize int64 = 512 << 20 // 512 MiB per archive
)

// unzip extracts a ZIP archive into dest, guarding against zip-slip and
// zip bombs (per-file and aggregate extraction caps, truncation detected
// rather than silently applied).
func unzip(r io.ReaderAt, size int, dest string) error {
	return unzipWithLimits(r, size, dest, maxUnzipFileSize, maxUnzipTotalSize)
}

func unzipWithLimits(r io.ReaderAt, size int, dest string, fileLimit, totalLimit int64) error {
	zr, err := zip.NewReader(r, int64(size))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	var total int64
	for _, f := range zr.File {
		if f.Name == "" || strings.Contains(f.Name, "..") {
			return fmt.Errorf("illegal archive path %q", f.Name)
		}
		path := filepath.Join(dest, filepath.Clean("/"+f.Name))
		if !strings.HasPrefix(path, filepath.Clean(dest)+string(os.PathSeparator)) && path != filepath.Clean(dest) {
			return fmt.Errorf("illegal archive path %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode().Perm()|0o200)
		if err != nil {
			rc.Close()
			return err
		}
		// Read one byte past the limit so truncation is detected instead
		// of silently producing a partial file.
		n, err := io.Copy(out, io.LimitReader(rc, fileLimit+1))
		rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if n > fileLimit {
			return fmt.Errorf("archive entry %q exceeds the per-file extraction limit of %d bytes", f.Name, fileLimit)
		}
		total += n
		if total > totalLimit {
			return fmt.Errorf("archive exceeds the total extraction limit of %d bytes", totalLimit)
		}
	}
	return nil
}

// validSlug reports whether slug is safe to use as a directory name:
// non-empty, not "." or "..", and only [A-Za-z0-9._-] (no separators).
func validSlug(slug string) bool {
	if slug == "" || slug == "." || slug == ".." {
		return false
	}
	for _, r := range slug {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}
