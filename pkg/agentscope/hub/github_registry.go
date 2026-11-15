package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/fsutil"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// GitHubRegistryDefaultBaseURL is GitHub's public MCP registry endpoint.
const GitHubRegistryDefaultBaseURL = "https://api.mcp.github.com"

// GitHubRegistryConfig configures the built-in GitHub MCP registry source
// (port of Python's GitHubMCPHub). The registry is public; a token only
// raises the rate limit.
type GitHubRegistryConfig struct {
	HubID       string          // default "github"
	DisplayName string          // default "GitHub MCP Registry"
	BaseURL     string          // default GitHubRegistryDefaultBaseURL
	APIToken    model.SecretStr // optional
	Timeout     time.Duration   // default 30s
}

// Compile-time interface check.
var _ Hub = (*GitHubMCPRegistry)(nil)

// GitHubMCPRegistry implements Hub over GitHub's MCP registry
// (GET /v0/servers). Card IDs are the registry server names, e.g.
// "io.github.upstash/context7".
type GitHubMCPRegistry struct {
	cfg    GitHubRegistryConfig
	client *http.Client
}

// NewGitHubMCPRegistry creates the hub with defaults applied.
func NewGitHubMCPRegistry(cfg GitHubRegistryConfig) *GitHubMCPRegistry {
	if cfg.HubID == "" {
		cfg.HubID = "github"
	}
	if cfg.DisplayName == "" {
		cfg.DisplayName = "GitHub MCP Registry"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = GitHubRegistryDefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &GitHubMCPRegistry{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}
}

func (h *GitHubMCPRegistry) ID() string          { return h.cfg.HubID }
func (h *GitHubMCPRegistry) DisplayName() string { return h.cfg.DisplayName }

// List browses the registry one cursor page at a time. The registry has
// no search parameter, so Query filters client-side against name and
// description (a page whose entries all filter out still carries the
// cursor — follow it).
func (h *GitHubMCPRegistry) List(ctx context.Context, opts *ListOptions) (*ListResult, error) {
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
	u, err := url.Parse(h.cfg.BaseURL + "/v0/servers")
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
		Servers  []map[string]any `json:"servers"`
		Metadata struct {
			NextCursor string `json:"next_cursor"`
		} `json:"metadata"`
	}
	if err := h.get(ctx, u.String(), &payload); err != nil {
		return nil, err
	}

	needle := strings.ToLower(opts.Query)
	cards := make([]Card, 0, len(payload.Servers))
	for _, entry := range payload.Servers {
		card, ok := h.toCard(entry)
		if !ok {
			continue // no reachable server (no remotes, no runnable packages)
		}
		if needle != "" &&
			!strings.Contains(strings.ToLower(card.Name), needle) &&
			!strings.Contains(strings.ToLower(card.Description), needle) {
			continue
		}
		cards = append(cards, card)
	}
	return &ListResult{
		Cards:      cards,
		NextCursor: payload.Metadata.NextCursor,
		HasMore:    payload.Metadata.NextCursor != "",
	}, nil
}

// Get fetches one registry entry by server name.
func (h *GitHubMCPRegistry) Get(ctx context.Context, cardID string) (*Card, error) {
	var payload map[string]any
	if err := h.get(ctx, h.cfg.BaseURL+"/v0/servers/"+url.PathEscape(cardID), &payload); err != nil {
		return nil, err
	}
	card, ok := h.toCard(payload)
	if !ok {
		return nil, fmt.Errorf("hub: registry entry %q describes no reachable server", cardID)
	}
	return &card, nil
}

// Install writes the server's MCP client configuration into targetDir as
// "<client-name>.json" shaped {"<client-name>": <config>}. Install inputs
// are PRESERVED: unknown values stay as "${KEY}" placeholders and the
// per-input specs (description / is_required / is_secret) are embedded
// under "inputs" so the application can fill them in later (Python #2230
// semantics — install must never drop the env surface).
//
// Note the deliberate shape divergence from MCPHub.Install, which persists
// the upstream registry's raw body as "<cardID>.json": each built-in hub
// writes its own upstream's native install shape.
func (h *GitHubMCPRegistry) Install(ctx context.Context, cardID string, targetDir string) error {
	card, err := h.Get(ctx, cardID)
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(map[string]any{card.Name: card.Config}, "", "  ")
	if err != nil {
		return fmt.Errorf("hub: marshal config: %w", err)
	}
	path := targetDir + "/" + card.Name + ".json"
	if err := fsutil.WriteFileAtomic(path, out, 0o644); err != nil {
		return fmt.Errorf("hub: write config: %w", err)
	}
	return nil
}

func (h *GitHubMCPRegistry) Close() error {
	h.client.CloseIdleConnections()
	return nil
}

func (h *GitHubMCPRegistry) get(ctx context.Context, rawURL string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("hub: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if token := h.cfg.APIToken.Value(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
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
		return fmt.Errorf("hub: registry returned status %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("hub: decode response: %w", err)
	}
	return nil
}

// runtimeArgs maps the registry's runtime_hint to the command that runs
// the package plus the arguments that precede the package spec (parity
// with Python's _RUNTIMES). Anything not listed here cannot be turned
// into a command, so the entry is skipped.
var runtimeArgs = map[string][]string{
	"npx":    {"-y"},
	"uvx":    {},
	"uv":     {"tool", "run"},
	"docker": {"run", "-i", "--rm"},
}

// placeholderPattern matches "{VAR}" placeholders as the registry writes
// them. Go's regexp engine has no lookbehind, so the "${VAR}-already"
// exclusion Python does with (?<!\$) is applied by the callers instead.
var placeholderPattern = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// substitutePlaceholders rewrites "{VAR}" to "${VAR}" (the form the
// renderer substitutes), leaving existing "${VAR}" untouched. Python
// parity: _substitute over string values.
func substitutePlaceholders(value any) string {
	s, ok := value.(string)
	if !ok {
		if value == nil {
			return ""
		}
		s = fmt.Sprint(value)
	}
	var b strings.Builder
	last := 0
	changed := false
	for _, m := range placeholderPattern.FindAllStringSubmatchIndex(s, -1) {
		start, end := m[0], m[1]
		if start > 0 && s[start-1] == '$' {
			continue // already "${VAR}"
		}
		b.WriteString(s[last:start])
		b.WriteString("${")
		b.WriteString(s[m[2]:m[3]])
		b.WriteString("}")
		last = end
		changed = true
	}
	if !changed {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// placeholderNames lists the "{VAR}" placeholder names a value carries,
// skipping ones already written as "${VAR}".
func placeholderNames(s string) []string {
	var names []string
	for _, m := range placeholderPattern.FindAllStringSubmatchIndex(s, -1) {
		if m[0] > 0 && s[m[0]-1] == '$' {
			continue
		}
		names = append(names, s[m[2]:m[3]])
	}
	return names
}

// fromRemote builds an HTTP config plus its install inputs from a
// "remotes" entry (port of Python's _from_remote). Header values keep
// their placeholders as "${VAR}", and every placeholder registers as a
// required install input (secret unless the entry says otherwise).
func fromRemote(remote map[string]any) (map[string]any, map[string]map[string]any) {
	inputs := map[string]map[string]any{}
	headers := map[string]any{}
	if hs, ok := remote["headers"].([]any); ok {
		for _, hh := range hs {
			header, _ := hh.(map[string]any)
			if header == nil {
				continue
			}
			value, _ := header["value"].(string)
			key, _ := header["name"].(string)
			if key == "" {
				// The registry omits the header name when the value
				// already spells out the scheme — bearer auth in practice.
				key = "Authorization"
			}
			headers[key] = substitutePlaceholders(value)
			desc, _ := header["description"].(string)
			for _, name := range placeholderNames(value) {
				secret := true
				if s, ok := header["is_secret"].(bool); ok {
					secret = s
				}
				inputs[name] = map[string]any{
					"description": desc,
					"is_required": true,
					"is_secret":   secret,
				}
			}
		}
	}
	config := map[string]any{"type": "http_mcp", "url": remote["url"]}
	if len(headers) > 0 {
		config["headers"] = headers
	}
	return config, inputs
}

// fromPackage builds a stdio config plus its install inputs from a
// "packages" entry (port of Python's _from_package). It reports false
// when the registry does not say how to run the package.
func fromPackage(pkg map[string]any) (map[string]any, map[string]map[string]any, bool) {
	runtime, _ := pkg["runtime_hint"].(string)
	prefix, ok := runtimeArgs[runtime]
	if !ok {
		return nil, nil, false
	}
	name, _ := pkg["name"].(string)
	if name == "" {
		return nil, nil, false
	}
	// Only npx takes a pinned spec ("name@version").
	spec := name
	if version, _ := pkg["version"].(string); version != "" && runtime == "npx" {
		spec = name + "@" + version
	}
	args := make([]string, 0, len(prefix)+1)
	args = append(args, prefix...)
	args = append(args, spec)

	env := map[string]any{}
	inputs := map[string]map[string]any{}
	if envVars, ok := pkg["environment_variables"].([]any); ok {
		for _, ev := range envVars {
			evm, _ := ev.(map[string]any)
			if evm == nil {
				continue
			}
			key, _ := evm["name"].(string)
			if key == "" {
				continue
			}
			nested, hasNested := evm["variables"].(map[string]any)
			value, hasValue := evm["value"]
			switch {
			case hasNested:
				env[key] = substitutePlaceholders(value)
				for inputName, inputSpec := range nested {
					if spec, ok := inputSpec.(map[string]any); ok {
						inputs[inputName] = spec
					}
				}
			case hasValue && value != nil:
				env[key] = fmt.Sprint(value)
			default:
				env[key] = "${" + key + "}"
				inputs[key] = evm
			}
		}
	}

	config := map[string]any{
		"type":    "stdio_mcp",
		"command": runtime,
		"args":    args,
	}
	if len(env) > 0 {
		config["env"] = env
	}
	return config, inputs, true
}

// toCard maps one registry entry onto a Card. The config template keeps
// the install-input surface: stdio package servers expose their inputs as
// "${KEY}" placeholders; remote servers carry their URL and auth headers.
//
// Deliberate deltas from Python's MCPCard: the Go Card is slimmer — no
// display_name/readme/updated_at fields (readmes run to tens of KB per
// entry and the Go app has no renderer for them yet).
func (h *GitHubMCPRegistry) toCard(entry map[string]any) (Card, bool) {
	server, _ := entry["server"].(map[string]any)
	if server == nil {
		return Card{}, false
	}
	github, _ := entry["x-github"].(map[string]any)

	var config map[string]any
	var inputs map[string]map[string]any

	remotes, _ := server["remotes"].([]any)
	for _, r := range remotes {
		remote, _ := r.(map[string]any)
		if remote == nil {
			continue
		}
		if remoteURL, _ := remote["url"].(string); remoteURL != "" {
			config, inputs = fromRemote(remote)
			break
		}
	}
	if config == nil {
		packages, _ := server["packages"].([]any)
		for _, p := range packages {
			pkg, _ := p.(map[string]any)
			if pkg == nil {
				continue
			}
			if cfg, ins, ok := fromPackage(pkg); ok {
				config, inputs = cfg, ins
				break
			}
		}
	}
	if config == nil {
		return Card{}, false
	}
	if len(inputs) > 0 {
		config["inputs"] = inputs
	}

	fullName, _ := server["name"].(string)
	name := fullName
	if i := strings.LastIndex(fullName, "/"); i >= 0 {
		name = fullName[i+1:] // keep the last segment
	}
	// Client names reach the model as mcp__{name}__{tool} and must match
	// [a-zA-Z0-9_-]+ (Python parity).
	name = strings.Trim(sanitizeClientName(name), "-")
	if name == "" {
		name = "mcp"
	}
	owner := ""
	if i := strings.LastIndex(fullName, "/"); i >= 0 {
		owner = fullName[:i]
	}
	desc, _ := server["description"].(string)

	card := Card{
		ID:          fullName,
		Owner:       owner,
		Kind:        CardKindMCP,
		Name:        name,
		Description: desc,
		Config:      config,
	}
	if versionDetail, ok := server["version_detail"].(map[string]any); ok {
		card.Version, _ = versionDetail["version"].(string)
	}
	if github != nil {
		if lang, ok := github["primary_language"].(string); ok && lang != "" {
			card.Tags = []string{lang}
		}
		if img, ok := github["preferred_image"].(string); ok && img != "" {
			card.IconURL = img
		} else if img, ok := github["owner_avatar_url"].(string); ok && img != "" {
			card.IconURL = img
		}
	}
	return card, true
}

// sanitizeClientName replaces every run of characters outside
// [a-zA-Z0-9_-] with a single dash (Python parity: re.sub collapses runs,
// so "a..b" → "a-b"; registry names like "io.github.x/context7" must
// become model-safe client names).
func sanitizeClientName(s string) string {
	var b strings.Builder
	pendingDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			if pendingDash {
				b.WriteByte('-')
			}
			pendingDash = false
			b.WriteRune(r)
		default:
			pendingDash = true
		}
	}
	if pendingDash {
		b.WriteByte('-')
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
