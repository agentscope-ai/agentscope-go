package hub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func skillArchiveBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	content := "# Skill"
	if err := tw.WriteHeader(&tar.Header{Name: "SKILL.md", Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()
	return buf.Bytes()
}

func TestSkillHub_InstallRejectsTraversalID(t *testing.T) {
	archive := skillArchiveBytes(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(archive)
	}))
	defer srv.Close()
	h := NewSkillHub(SkillHubConfig{BaseURL: srv.URL})
	defer h.Close()

	parent := t.TempDir()
	dir := filepath.Join(parent, "target")
	for _, id := range []string{"../escaped", "..", "a/b", ""} {
		if err := h.Install(context.Background(), id, dir); err == nil {
			t.Errorf("install of %q must be rejected", id)
		}
	}
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if e.Name() != "target" {
			t.Errorf("rejected install wrote outside target: %s", e.Name())
		}
	}
}

func TestMCPHub_InstallRejectsTraversalID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"name":"x"}`))
	}))
	defer srv.Close()
	h := NewMCPHub(MCPHubConfig{BaseURL: srv.URL})
	defer h.Close()

	parent := t.TempDir()
	dir := filepath.Join(parent, "target")
	for _, id := range []string{"../escaped", "..", "a/b", ""} {
		if err := h.Install(context.Background(), id, dir); err == nil {
			t.Errorf("install of %q must be rejected", id)
		}
	}
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if e.Name() != "target" {
			t.Errorf("rejected install wrote outside target: %s", e.Name())
		}
	}
}
