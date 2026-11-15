package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestWorkspaceManager_SharingAndRefcounts(t *testing.T) {
	m := NewWorkspaceManager(t.TempDir(), "local")

	// Private workspaces named after their session.
	if _, err := m.GetOrCreate("s1"); err != nil {
		t.Fatal(err)
	}
	if id, ok := m.BoundWorkspaceID("s1"); !ok || id != "s1" {
		t.Errorf("s1 must bind to its private workspace, got %q %v", id, ok)
	}
	if m.RefCount("s1") != 1 {
		t.Errorf("private refcount must be 1, got %d", m.RefCount("s1"))
	}

	// Two sessions share one named workspace.
	shared, err := m.Share("s1", "team")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Share("s2", "team"); err != nil {
		t.Fatal(err)
	}
	if m.RefCount("team") != 2 {
		t.Errorf("team refcount must be 2, got %d", m.RefCount("team"))
	}
	if m.GetByID("team") == nil {
		t.Error("team workspace must be live")
	}
	// s1's private workspace was released when it moved to team.
	if m.GetByID("s1") != nil || m.RefCount("s1") != 0 {
		t.Error("s1 private workspace must be released after rebinding")
	}

	// Same workspace object is shared.
	if m.GetByID("team") != shared {
		t.Error("share must return the same workspace instance")
	}

	// Releasing drops the workspace only when the last session unbinds.
	m.Remove("s1")
	if m.RefCount("team") != 1 || m.GetByID("team") == nil {
		t.Error("team must survive while s2 is bound")
	}
	m.Remove("s2")
	if m.GetByID("team") != nil || m.RefCount("team") != 0 {
		t.Error("team must be released after its last session unbinds")
	}

	// Files persist: re-sharing recreates over the same dir.
	if err := shared.WriteFile(context.Background(), "marker.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	again, err := m.Share("s3", "team")
	if err != nil {
		t.Fatal(err)
	}
	data, err := again.ReadFile(context.Background(), "marker.txt")
	if err != nil || string(data) != "x" {
		t.Errorf("recreated workspace must see persisted files, got %q %v", data, err)
	}

	// Invalid workspace IDs are rejected.
	for _, id := range []string{"", ".", "..", ".hidden", "a/b", "a\\b"} {
		if _, err := m.Share("s4", id); err == nil {
			t.Errorf("share with id %q must fail", id)
		}
	}
}

func newSharingTestApp(t *testing.T) (*httptest.Server, *App) {
	t.Helper()
	app, err := CreateApp(&AppConfig{
		WorkspaceDir: t.TempDir(),
		AgentFactory: testAgentFactory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	return srv, app
}

func TestWorkspaceShareRoutes(t *testing.T) {
	srv, app := newSharingTestApp(t)

	// Seed a live shared workspace with content.
	ws, err := app.wsMgr.Share("s1", "team")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile(context.Background(), "notes/hello.txt", []byte("hello artifact")); err != nil {
		t.Fatal(err)
	}

	// Share endpoint rebinds another session onto it.
	resp, err := http.Post(srv.URL+"/api/workspace/share", "application/json",
		bytes.NewReader([]byte(`{"session_id":"s2","workspace_id":"team"}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("share must be 200, got %d", resp.StatusCode)
	}
	if app.wsMgr.RefCount("team") != 2 {
		t.Errorf("share route must bind, refcount = %d", app.wsMgr.RefCount("team"))
	}

	// Missing fields → 400.
	resp, _ = http.Post(srv.URL+"/api/workspace/share", "application/json", bytes.NewReader([]byte(`{}`)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty share body must be 400, got %d", resp.StatusCode)
	}

	// Invalid workspace id → 400.
	resp, _ = http.Post(srv.URL+"/api/workspace/share", "application/json",
		bytes.NewReader([]byte(`{"session_id":"s2","workspace_id":"a/b"}`)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid workspace id must be 400, got %d", resp.StatusCode)
	}

	// list_dir on the workspace root and a subdirectory.
	for _, u := range []string{
		srv.URL + "/api/workspace/team/list_dir",
		srv.URL + "/api/workspace/team/list_dir?path=notes",
	} {
		r, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("list_dir %s must be 200, got %d: %s", u, r.StatusCode, body)
		}
	}

	// read_file returns the content.
	r, err := http.Get(srv.URL + "/api/workspace/team/read_file?path=notes/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || string(body) != "hello artifact" {
		t.Fatalf("read_file wrong: %d %q", r.StatusCode, body)
	}
	if r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff header missing")
	}

	// Traversal is blocked.
	r, _ = http.Get(srv.URL + "/api/workspace/team/read_file?path=../../etc/passwd")
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("traversal must be 400, got %d", r.StatusCode)
	}
	r, _ = http.Get(srv.URL + "/api/workspace/team/list_dir?path=../..")
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("list traversal must be 400, got %d", r.StatusCode)
	}

	// Unknown workspace → 404; missing file → 404; directory read → 400.
	r, _ = http.Get(srv.URL + "/api/workspace/ghost/list_dir")
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown workspace must be 404, got %d", r.StatusCode)
	}
	r, _ = http.Get(srv.URL + "/api/workspace/team/read_file?path=missing.txt")
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("missing file must be 404, got %d", r.StatusCode)
	}
	r, _ = http.Get(srv.URL + "/api/workspace/team/read_file?path=notes")
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("reading a directory must be 400, got %d", r.StatusCode)
	}
}

func TestWorkspaceShareRoutes_SizeCap(t *testing.T) {
	srv, app := newSharingTestApp(t)
	ws, err := app.wsMgr.Share("s1", "big")
	if err != nil {
		t.Fatal(err)
	}
	// One byte over the cap.
	if err := ws.WriteFile(context.Background(), "big.bin", bytes.Repeat([]byte("a"), maxArtifactSize+1)); err != nil {
		t.Fatal(err)
	}
	r, err := http.Get(srv.URL + "/api/workspace/big/read_file?path=big.bin")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized file must be 413, got %d", r.StatusCode)
	}
	if !strings.HasPrefix(srv.URL, "http") {
		t.Fatal("unreachable")
	}
}

func TestWorkspaceManager_ConcurrentAccess(t *testing.T) {
	m := NewWorkspaceManager(t.TempDir(), "local")

	// Concurrent GetOrCreate of the same session must converge on one
	// instance with a single reference.
	results := make([]any, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ws, err := m.GetOrCreate("s1")
			if err == nil {
				results[n] = ws
			}
		}(i)
	}
	wg.Wait()
	if results[0] == nil {
		t.Fatal("concurrent GetOrCreate failed")
	}
	for _, ws := range results {
		if ws != results[0] {
			t.Fatal("concurrent creators must adopt one shared instance")
		}
	}
	if got := m.RefCount("s1"); got != 1 {
		t.Errorf("refcount must be exactly 1, got %d", got)
	}

	// Concurrent Share onto one workspace from distinct sessions.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if _, err := m.Share(fmt.Sprintf("c%d", n), "team"); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if got := m.RefCount("team"); got != 20 {
		t.Errorf("team refcount must be 20, got %d", got)
	}
}

func TestWorkspaceManager_RebindStorm(t *testing.T) {
	m := NewWorkspaceManager(t.TempDir(), "local")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _ = m.GetOrCreate("hot")
		}()
		go func(n int) {
			defer wg.Done()
			_, _ = m.Share("hot", fmt.Sprintf("w%d", n%5))
		}(i)
		go func() {
			defer wg.Done()
			m.Remove("hot")
		}()
	}
	wg.Wait()

	// Quiescence: one more GetOrCreate must leave a consistent state —
	// the binding points at a live workspace with at least one reference.
	if _, err := m.GetOrCreate("hot"); err != nil {
		t.Fatal(err)
	}
	id, ok := m.BoundWorkspaceID("hot")
	if !ok {
		t.Fatal("hot must be bound after quiescence")
	}
	if m.GetByID(id) == nil {
		t.Fatalf("binding %q must point at a live workspace", id)
	}
	if got := m.RefCount(id); got < 1 {
		t.Errorf("bound workspace %q must have refs >= 1, got %d", id, got)
	}
}
