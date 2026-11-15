package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const githubListFixture = `{
	"servers": [
		{
			"server": {
				"name": "io.github.upstash/context7",
				"description": "Up-to-date documentation for libraries",
				"remotes": [{"url": "https://mcp.context7.com/mcp"}],
				"version_detail": {"version": "0.2.1"}
			},
			"x-github": {"preferred_image": "https://example/icon.png", "primary_language": "TypeScript"}
		},
		{
			"server": {
				"name": "io.modelcontextprotocol.server.everart",
				"description": "AI image generation",
				"packages": [{
					"runtime_hint": "npx",
					"name": "everart-mcp-server",
					"version": "1.0.0",
					"environment_variables": [{"name": "EVERART_API_KEY", "description": "EverArt API key"}]
				}]
			}
		},
		{
			"server": {
				"name": "com.example.docker-mcp",
				"description": "Docker runtime entry",
				"packages": [{"runtime_hint": "docker", "name": "example/mcp-server", "version": "2.0"}]
			}
		},
		{
			"server": {
				"name": "com.example.uvx-mcp",
				"description": "Uvx runtime entry",
				"packages": [{"runtime_hint": "uvx", "name": "uvx-mcp-server", "version": "3.0"}]
			}
		},
		{
			"server": {
				"name": "com.example.authed-remote",
				"description": "Remote with bearer header",
				"remotes": [{
					"url": "https://mcp.example.com/mcp",
					"headers": [{"value": "{TOKEN}", "description": "API token", "is_secret": true}]
				}]
			}
		},
		{
			"server": {
				"name": "com.example.hintless",
				"description": "Package without runtime hint — skipped",
				"packages": [{"name": "some-package"}]
			}
		},
		{
			"server": {"name": "broken/one", "description": "no remotes, no packages"}
		}
	],
	"metadata": {"next_cursor": "page-2"}
}`

func newGitHubTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/servers":
			if r.URL.Query().Get("limit") == "" {
				t.Errorf("limit param missing")
			}
			_, _ = w.Write([]byte(githubListFixture))
		case strings.HasPrefix(r.URL.Path, "/v0/servers/"):
			if strings.Contains(r.URL.Path, "context7") {
				_, _ = w.Write([]byte(`{"server": {"name": "io.github.upstash/context7", "description": "docs", "remotes": [{"url": "https://mcp.context7.com/mcp"}]}}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestGitHubMCPRegistry_List(t *testing.T) {
	srv := newGitHubTestServer(t)
	defer srv.Close()
	h := NewGitHubMCPRegistry(GitHubRegistryConfig{BaseURL: srv.URL})
	defer h.Close()

	if h.ID() != "github" || h.DisplayName() == "" {
		t.Errorf("defaults wrong: %s / %s", h.ID(), h.DisplayName())
	}

	res, err := h.List(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Usable: context7, everart, docker, uvx, authed-remote. Skipped:
	// hintless package, broken entry.
	if len(res.Cards) != 5 {
		t.Fatalf("expected 5 usable cards, got %d: %+v", len(res.Cards), res.Cards)
	}
	if res.NextCursor != "page-2" || !res.HasMore {
		t.Errorf("cursor not propagated: %+v", res)
	}

	byName := map[string]Card{}
	for _, c := range res.Cards {
		byName[c.Name] = c
	}

	remote := byName["context7"]
	if remote.ID != "io.github.upstash/context7" || remote.Owner != "io.github.upstash" {
		t.Errorf("remote card mapping wrong: %+v", remote)
	}
	if remote.Config["type"] != "http_mcp" || !strings.Contains(remote.Config["url"].(string), "context7") {
		t.Errorf("remote config wrong: %+v", remote.Config)
	}
	if remote.Version != "0.2.1" || len(remote.Tags) != 1 || remote.Tags[0] != "TypeScript" {
		t.Errorf("version/tags not mapped: %+v", remote)
	}

	pkg := byName["io-modelcontextprotocol-server-everart"]
	if pkg.Config["type"] != "stdio_mcp" || pkg.Config["command"] != "npx" {
		t.Errorf("package config wrong: %+v", pkg.Config)
	}
	if want := []string{"-y", "everart-mcp-server@1.0.0"}; !reflect.DeepEqual(pkg.Config["args"], want) {
		t.Errorf("npx args must pin the version, got %v", pkg.Config["args"])
	}
	env, _ := pkg.Config["env"].(map[string]any)
	if env["EVERART_API_KEY"] != "${EVERART_API_KEY}" {
		t.Errorf("env input placeholder not preserved: %+v", pkg.Config)
	}
	inputs, _ := pkg.Config["inputs"].(map[string]map[string]any)
	if inputs["EVERART_API_KEY"]["description"] != "EverArt API key" {
		t.Errorf("input spec not embedded: %+v", pkg.Config["inputs"])
	}

	docker := byName["com-example-docker-mcp"]
	if want := []string{"run", "-i", "--rm", "example/mcp-server"}; !reflect.DeepEqual(docker.Config["args"], want) {
		t.Errorf("docker args wrong, got %v", docker.Config["args"])
	}

	// uvx takes no flags and no version pin (parity with Python _RUNTIMES).
	uvx := byName["com-example-uvx-mcp"]
	if want := []string{"uvx-mcp-server"}; !reflect.DeepEqual(uvx.Config["args"], want) {
		t.Errorf("uvx args wrong, got %v", uvx.Config["args"])
	}
	if uvx.Config["command"] != "uvx" {
		t.Errorf("uvx command wrong: %+v", uvx.Config)
	}

	// Remote auth headers become config headers + install inputs.
	authed := byName["com-example-authed-remote"]
	headers, _ := authed.Config["headers"].(map[string]any)
	if headers["Authorization"] != "${TOKEN}" {
		t.Errorf("auth header placeholder not rewritten: %+v", authed.Config)
	}
	authInputs, _ := authed.Config["inputs"].(map[string]map[string]any)
	token, ok := authInputs["TOKEN"]
	if !ok || token["is_required"] != true || token["is_secret"] != true || token["description"] != "API token" {
		t.Errorf("auth input spec wrong: %+v", authed.Config["inputs"])
	}

	// Client-side query filter (the registry has no search parameter).
	filtered, err := h.List(context.Background(), &ListOptions{Query: "image"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Cards) != 1 || filtered.Cards[0].Name != "io-modelcontextprotocol-server-everart" {
		t.Errorf("query filter wrong: %+v", filtered.Cards)
	}
	if filtered.NextCursor != "page-2" {
		t.Error("filtered page must still carry the cursor")
	}
}

func TestGitHubMCPRegistry_GetAndInstallPreservesInputs(t *testing.T) {
	srv := newGitHubTestServer(t)
	defer srv.Close()
	h := NewGitHubMCPRegistry(GitHubRegistryConfig{BaseURL: srv.URL})
	defer h.Close()

	card, err := h.Get(context.Background(), "io.github.upstash/context7")
	if err != nil {
		t.Fatal(err)
	}
	if card.Name != "context7" {
		t.Errorf("get mapping wrong: %+v", card)
	}

	if _, err := h.Get(context.Background(), "missing/one"); err == nil {
		t.Error("missing card must error")
	}

	dir := t.TempDir()
	if err := h.Install(context.Background(), "io.github.upstash/context7", dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "context7.json"))
	if err != nil {
		t.Fatal(err)
	}
	var installed map[string]map[string]any
	if err := json.Unmarshal(raw, &installed); err != nil {
		t.Fatal(err)
	}
	cfg := installed["context7"]
	if cfg == nil || !strings.Contains(cfg["url"].(string), "context7") {
		t.Errorf("installed config wrong: %s", raw)
	}
}

func TestGitHubMCPRegistry_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer srv.Close()
	h := NewGitHubMCPRegistry(GitHubRegistryConfig{BaseURL: srv.URL})
	defer h.Close()
	if _, err := h.List(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("expected 403 error, got %v", err)
	}
}

func TestSubstitutePlaceholders(t *testing.T) {
	cases := map[string]string{
		"{TOKEN}":        "${TOKEN}",
		"${TOKEN}":       "${TOKEN}",        // already rewritten — untouched
		"Bearer {TOKEN}": "Bearer ${TOKEN}", // mid-string
		"{A} and {B}":    "${A} and ${B}",   // several
		"plain":          "plain",           // none
		"{bad-name}":     "{bad-name}",      // not an identifier — untouched
	}
	for in, want := range cases {
		if got := substitutePlaceholders(in); got != want {
			t.Errorf("substitutePlaceholders(%q) = %q, want %q", in, got, want)
		}
	}
	if got := substitutePlaceholders(nil); got != "" {
		t.Errorf("nil must substitute to empty, got %q", got)
	}
	if names := placeholderNames("{A} ${B} {C}"); !reflect.DeepEqual(names, []string{"A", "C"}) {
		t.Errorf("placeholderNames wrong: %v", names)
	}
}

func TestFromPackageRuntimeMatrix(t *testing.T) {
	cases := []struct {
		runtime string
		version string
		want    []string
		ok      bool
	}{
		{"npx", "1.2.3", []string{"-y", "pkg@1.2.3"}, true},
		{"npx", "", []string{"-y", "pkg"}, true},
		{"uvx", "1.2.3", []string{"pkg"}, true},
		{"uv", "", []string{"tool", "run", "pkg"}, true},
		{"docker", "2.0", []string{"run", "-i", "--rm", "pkg"}, true},
		{"cargo", "", nil, false},
		{"", "", nil, false},
	}
	for _, tc := range cases {
		pkg := map[string]any{"runtime_hint": tc.runtime, "name": "pkg"}
		if tc.version != "" {
			pkg["version"] = tc.version
		}
		config, _, ok := fromPackage(pkg)
		if ok != tc.ok {
			t.Errorf("runtime %q: ok = %v, want %v", tc.runtime, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if !reflect.DeepEqual(config["args"], tc.want) {
			t.Errorf("runtime %q: args = %v, want %v", tc.runtime, config["args"], tc.want)
		}
	}
	// Nested "variables" merge their specs into the inputs.
	config, inputs, ok := fromPackage(map[string]any{
		"runtime_hint": "npx",
		"name":         "pkg",
		"environment_variables": []any{map[string]any{
			"name":      "GROUP",
			"value":     "{INNER}",
			"variables": map[string]any{"INNER": map[string]any{"description": "nested", "is_secret": true}},
		}},
	})
	if !ok {
		t.Fatal("nested env must build")
	}
	if env, _ := config["env"].(map[string]any); env["GROUP"] != "${INNER}" {
		t.Errorf("nested env value not substituted: %+v", config["env"])
	}
	if inputs["INNER"]["description"] != "nested" || inputs["INNER"]["is_secret"] != true {
		t.Errorf("nested input spec not merged: %+v", inputs)
	}
}

func TestSanitizeClientNameCollapsesRuns(t *testing.T) {
	cases := map[string]string{
		"a..b":            "a-b",      // run collapses (Python re.sub parity)
		"a/b":             "a-b",      // separators too
		"my--name":        "my--name", // literal dashes stay
		"x..-y":           "x--y",     // run dash + literal dash
		"context7":        "context7",
		"a..b//c":         "a-b-c",
		"..lead..trail..": "-lead-trail-", // caller trims the edge dashes
		"///":             "-",            // exact re.sub parity
	}
	for in, want := range cases {
		if got := sanitizeClientName(in); got != want {
			t.Errorf("sanitizeClientName(%q) = %q, want %q", in, got, want)
		}
	}
}
