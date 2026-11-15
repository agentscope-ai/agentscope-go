package hub

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newClawTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/skills", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"items": [
				{"slug": "gifgrep", "summary": "Search GIFs", "topics": ["fun"], "version": "1.2.3"},
				{"slug": "collab", "summary": "Owned skill", "ownerHandle": "alice"}
			],
			"nextCursor": "c2"
		}`))
	})
	mux.HandleFunc("/api/v1/search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "" {
			t.Error("search without q")
		}
		_, _ = w.Write([]byte(`{"results": [{"slug": "gifgrep", "summary": "Search GIFs", "ownerHandle": "alice", "version": "2.0.0"}]}`))
	})
	mux.HandleFunc("/api/v1/skills/gifgrep", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ownerHandle") == "" {
			_, _ = w.Write([]byte(`{"skill": {"slug": "gifgrep", "summary": "ambiguous", "latestVersion": {"version": "9.9.9"}}, "owner": {"handle": "bob"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"skill": {"slug": "gifgrep", "summary": "scoped", "latestVersion": {"version": "1.0.0"}}, "owner": {"handle": "alice"}}`))
	})
	mux.HandleFunc("/api/v1/download", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("slug") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		f, _ := zw.Create("SKILL.md")
		_, _ = f.Write([]byte("# gifgrep skill"))
		f2, _ := zw.Create("sub/notes.txt")
		_, _ = f2.Write([]byte("nested"))
		_ = zw.Close()
		_, _ = w.Write(buf.Bytes())
	})
	return httptest.NewServer(mux)
}

func TestClawHub_ListCatalogAndSearch(t *testing.T) {
	srv := newClawTestServer(t)
	defer srv.Close()
	h := NewClawHub(ClawHubConfig{BaseURL: srv.URL})
	defer h.Close()

	if h.ID() != "clawhub" || h.DisplayName() == "" {
		t.Errorf("defaults wrong: %s / %s", h.ID(), h.DisplayName())
	}

	res, err := h.List(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cards) != 2 {
		t.Fatalf("expected 2 cards, got %d", len(res.Cards))
	}
	if res.NextCursor != "c2" || !res.HasMore {
		t.Errorf("cursor wrong: %+v", res)
	}
	// Bare slug when the record names no owner; owner-scoped when it does.
	if res.Cards[0].ID != "gifgrep" {
		t.Errorf("catalog card without owner must be a bare slug, got %q", res.Cards[0].ID)
	}
	if res.Cards[1].ID != "alice/collab" || res.Cards[1].Owner != "alice" {
		t.Errorf("owner-scoped id wrong: %+v", res.Cards[1])
	}
	if res.Cards[0].Version != "1.2.3" || len(res.Cards[0].Tags) != 1 {
		t.Errorf("version/tags wrong: %+v", res.Cards[0])
	}

	search, err := h.List(context.Background(), &ListOptions{Query: "gif"})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Cards) != 1 || search.Cards[0].ID != "alice/gifgrep" || search.HasMore {
		t.Errorf("search result wrong: %+v", search)
	}
}

func TestClawHub_GetScopedAndAmbiguous(t *testing.T) {
	srv := newClawTestServer(t)
	defer srv.Close()
	h := NewClawHub(ClawHubConfig{BaseURL: srv.URL})
	defer h.Close()

	scoped, err := h.Get(context.Background(), "alice/gifgrep")
	if err != nil {
		t.Fatal(err)
	}
	if scoped.Description != "scoped" || scoped.Owner != "alice" {
		t.Errorf("scoped get wrong: %+v", scoped)
	}

	ambiguous, err := h.Get(context.Background(), "gifgrep")
	if err != nil {
		t.Fatal(err)
	}
	if ambiguous.Description != "ambiguous" || ambiguous.ID != "bob/gifgrep" {
		t.Errorf("bare get wrong: %+v", ambiguous)
	}
}

func TestClawHub_InstallUnpacksZip(t *testing.T) {
	srv := newClawTestServer(t)
	defer srv.Close()
	h := NewClawHub(ClawHubConfig{BaseURL: srv.URL})
	defer h.Close()

	dir := t.TempDir()
	if err := h.Install(context.Background(), "alice/gifgrep", dir); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "gifgrep", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "gifgrep skill") {
		t.Errorf("unexpected content: %s", content)
	}
	if _, err := os.Stat(filepath.Join(dir, "gifgrep", "sub", "notes.txt")); err != nil {
		t.Errorf("nested file missing: %v", err)
	}
}

func TestClawHub_UnzipRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("../evil.txt")
	_, _ = f.Write([]byte("pwned"))
	_ = zw.Close()

	dir := t.TempDir()
	err := unzip(bytes.NewReader(buf.Bytes()), buf.Len(), filepath.Join(dir, "dest"))
	if err == nil {
		t.Fatal("zip-slip must be rejected")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "evil.txt")); statErr == nil {
		t.Fatal("evil file must not exist outside dest")
	}
}

func TestSplitClawCardID(t *testing.T) {
	if slug, owner := splitClawCardID("alice/gifgrep"); slug != "gifgrep" || owner != "alice" {
		t.Errorf("split wrong: %s %s", slug, owner)
	}
	if slug, owner := splitClawCardID("gifgrep"); slug != "gifgrep" || owner != "" {
		t.Errorf("bare split wrong: %s %s", slug, owner)
	}
	if slug, owner := splitClawCardID("a/b/c"); slug != "c" || owner != "a/b" {
		t.Errorf("nested split wrong: %s %s", slug, owner)
	}
}

func TestClawHub_InstallRejectsBadSlug(t *testing.T) {
	srv := newClawTestServer(t)
	defer srv.Close()
	h := NewClawHub(ClawHubConfig{BaseURL: srv.URL})
	defer h.Close()

	dir := t.TempDir()
	for _, id := range []string{"..", "alice/..", "alice/with space", ""} {
		if err := h.Install(context.Background(), id, dir); err == nil {
			t.Errorf("install of %q must be rejected", id)
		}
	}
	// Nothing may have been created outside the target dir.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("rejected installs must not write, found %v", entries)
	}
}

func TestValidSlug(t *testing.T) {
	for _, ok := range []string{"gifgrep", "my-skill.v2", "A_B-9"} {
		if !validSlug(ok) {
			t.Errorf("validSlug(%q) must be true", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "a b", "a\\b", "..x/"} {
		if validSlug(bad) {
			t.Errorf("validSlug(%q) must be false", bad)
		}
	}
}

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUnzipWithLimitsPerFileCap(t *testing.T) {
	data := makeZip(t, map[string]string{"big.bin": strings.Repeat("a", 100)})
	dir := t.TempDir()
	err := unzipWithLimits(bytes.NewReader(data), len(data), dir, 64, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "per-file") {
		t.Fatalf("expected per-file limit error, got %v", err)
	}
}

func TestUnzipWithLimitsTotalCap(t *testing.T) {
	data := makeZip(t, map[string]string{
		"a.bin": strings.Repeat("a", 40),
		"b.bin": strings.Repeat("b", 40),
		"c.bin": strings.Repeat("c", 40),
	})
	dir := t.TempDir()
	err := unzipWithLimits(bytes.NewReader(data), len(data), dir, 64, 100)
	if err == nil || !strings.Contains(err.Error(), "total extraction limit") {
		t.Fatalf("expected total limit error, got %v", err)
	}

	// Same archive under the caps extracts fine.
	dir2 := t.TempDir()
	if err := unzipWithLimits(bytes.NewReader(data), len(data), dir2, 64, 1<<20); err != nil {
		t.Fatalf("under-cap extraction failed: %v", err)
	}
	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		if _, err := os.Stat(filepath.Join(dir2, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}
