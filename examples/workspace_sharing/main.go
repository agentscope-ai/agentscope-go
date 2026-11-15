package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/app"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/workspace"
)

// This example demonstrates session-workspace sharing and the read-only
// artifact endpoints (Phase 3, Python #1951/#2187 semantics):
//
//   - POST /api/workspace/share            binds sessions onto one named
//     workspace (refcounted; released when the last session unbinds).
//   - GET  /api/workspace/{id}/list_dir    browse a live workspace.
//   - GET  /api/workspace/{id}/read_file   read one file (jail-enforced,
//     10 MiB pre-read cap).
//
// Agents that need the workspace at construction time use
// AppConfig.WorkspaceAgentFactory (the session's workspace is handed to
// the factory — the hook for filesystem-backed middleware such as
// memory.NewAgenticMemory). This demo exercises the HTTP surface, so the
// plain factory is never called and no API key is needed.

func main() {
	baseDir, err := os.MkdirTemp("", "agentscope-workspaces-*")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer os.RemoveAll(baseDir)

	a, err := app.CreateApp(&app.AppConfig{
		Addr:         "127.0.0.1:0",
		WorkspaceDir: baseDir,
		AgentFactory: func(_ *app.SessionRecord) (*agent.UnifiedAgent, error) {
			return nil, fmt.Errorf("not needed in this example")
		},
	})
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	srv := &http.Server{Handler: a.Handler()}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown(context.Background())
	base := "http://" + ln.Addr().String()
	fmt.Println("App listening on", base)

	// ---------- Two sessions share one workspace ----------
	share := func(sessionID, workspaceID string) {
		body, _ := json.Marshal(map[string]string{
			"session_id": sessionID, "workspace_id": workspaceID,
		})
		resp, err := http.Post(base+"/api/workspace/share", "application/json", bytes.NewReader(body))
		if err != nil {
			panic(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		fmt.Printf("share %-3s -> %-5s : %d %s\n", sessionID, workspaceID, resp.StatusCode, bytes.TrimSpace(raw))
	}
	share("s1", "team")
	share("s2", "team")

	// ---------- Seed an artifact (files live at <baseDir>/<workspaceID>) ----------
	ws, err := workspace.NewLocalWorkspace(workspace.LocalConfig{
		BasePath: filepath.Join(baseDir, "team"),
	})
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	if err := ws.WriteFile(context.Background(), "notes/hello.md", []byte("# Team notes\n\nShip it on Friday.\n")); err != nil {
		fmt.Println("Error:", err)
		return
	}

	// ---------- Browse + read through the artifact endpoints ----------
	get := func(url string) {
		resp, err := http.Get(url)
		if err != nil {
			panic(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		fmt.Printf("GET %s\n  -> %d %s\n", url[len(base):], resp.StatusCode, bytes.TrimSpace(raw))
	}
	get(base + "/api/workspace/team/list_dir")
	get(base + "/api/workspace/team/list_dir?path=notes")
	get(base + "/api/workspace/team/read_file?path=notes/hello.md")

	// ---------- The jail rejects traversal ----------
	get(base + "/api/workspace/team/read_file?path=../../etc/passwd")
	get(base + "/api/workspace/ghost/list_dir")

	fmt.Println("\nDone. The 'team' workspace was shared by 2 sessions;")
	fmt.Println("artifact access stayed inside", filepath.Join(baseDir, "team"))
}
