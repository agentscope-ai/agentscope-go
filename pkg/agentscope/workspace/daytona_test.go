package workspace

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDaytonaInterfaceCompliance(t *testing.T) {
	var _ Workspace = (*DaytonaWorkspace)(nil)
}

func TestNewDaytonaWorkspace_MissingBaseURL(t *testing.T) {
	_, err := NewDaytonaWorkspace(DaytonaConfig{
		APIKey:      "key",
		WorkspaceID: "ws-1",
	})
	if err == nil {
		t.Fatal("expected error for missing base URL")
	}
	if !strings.Contains(err.Error(), "base URL") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewDaytonaWorkspace_MissingAPIKey(t *testing.T) {
	_, err := NewDaytonaWorkspace(DaytonaConfig{
		BaseURL:     "http://localhost",
		WorkspaceID: "ws-1",
	})
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
	if !strings.Contains(err.Error(), "API key") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewDaytonaWorkspace_MissingWorkspaceID(t *testing.T) {
	_, err := NewDaytonaWorkspace(DaytonaConfig{
		BaseURL: "http://localhost",
		APIKey:  "key",
	})
	if err == nil {
		t.Fatal("expected error for missing workspace ID")
	}
	if !strings.Contains(err.Error(), "workspace ID") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewDaytonaWorkspace_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/workspace/") {
			// Verify auth header.
			auth := r.Header.Get("Authorization")
			if auth != "Bearer test-key" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"ws-123","state":"running"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ws, err := NewDaytonaWorkspace(DaytonaConfig{
		BaseURL:     srv.URL,
		APIKey:      "test-key",
		WorkspaceID: "ws-123",
	})
	if err != nil {
		t.Fatalf("NewDaytonaWorkspace: %v", err)
	}
	if ws.BasePath() != "/home/daytona" {
		t.Errorf("BasePath = %q, want /home/daytona", ws.BasePath())
	}
}

func TestNewDaytonaWorkspace_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "workspace not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := NewDaytonaWorkspace(DaytonaConfig{
		BaseURL:     srv.URL,
		APIKey:      "key",
		WorkspaceID: "nonexistent",
	})
	if err == nil {
		t.Fatal("expected error for non-existent workspace")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDaytonaExecute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/workspace/ws-1"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/toolbox/process/execute"):
			// Verify the request body.
			var req daytonaExecRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if req.Command != "echo hello" {
				t.Errorf("command = %q, want %q", req.Command, "echo hello")
			}
			resp := daytonaExecResponse{
				Stdout:   "hello\n",
				Stderr:   "",
				ExitCode: 0,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ws, err := NewDaytonaWorkspace(DaytonaConfig{
		BaseURL:     srv.URL,
		APIKey:      "key",
		WorkspaceID: "ws-1",
	})
	if err != nil {
		t.Fatalf("NewDaytonaWorkspace: %v", err)
	}

	result, err := ws.Execute(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Stdout != "hello\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "hello\n")
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}

func TestDaytonaExecute_NonZeroExit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/workspace/"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/toolbox/process/execute"):
			resp := daytonaExecResponse{
				Stdout:   "",
				Stderr:   "command not found",
				ExitCode: 127,
			}
			json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ws, err := NewDaytonaWorkspace(DaytonaConfig{
		BaseURL:     srv.URL,
		APIKey:      "key",
		WorkspaceID: "ws-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	result, err := ws.Execute(context.Background(), "badcmd")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 127 {
		t.Errorf("ExitCode = %d, want 127", result.ExitCode)
	}
	if result.Stderr != "command not found" {
		t.Errorf("Stderr = %q, want %q", result.Stderr, "command not found")
	}
}

func TestDaytonaExecute_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/workspace/"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost:
			http.Error(w, "internal error", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ws, err := NewDaytonaWorkspace(DaytonaConfig{
		BaseURL:     srv.URL,
		APIKey:      "key",
		WorkspaceID: "ws-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, err = ws.Execute(context.Background(), "ls")
	if err == nil {
		t.Fatal("expected error for server error")
	}
}

func TestDaytonaWriteFile(t *testing.T) {
	var capturedPath string
	var capturedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/workspace/ws-1") && !strings.Contains(r.URL.Path, "toolbox"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/toolbox/files"):
			capturedPath = r.URL.Query().Get("path")
			var err error
			capturedBody, err = io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read error", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ws, err := NewDaytonaWorkspace(DaytonaConfig{
		BaseURL:     srv.URL,
		APIKey:      "key",
		WorkspaceID: "ws-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	err = ws.WriteFile(context.Background(), "test.txt", []byte("file content"))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if capturedPath != "/home/daytona/test.txt" {
		t.Errorf("path = %q, want /home/daytona/test.txt", capturedPath)
	}
	if string(capturedBody) != "file content" {
		t.Errorf("body = %q, want %q", string(capturedBody), "file content")
	}
}

func TestDaytonaWriteFile_AbsolutePath(t *testing.T) {
	var capturedPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/workspace/"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/toolbox/files"):
			capturedPath = r.URL.Query().Get("path")
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ws, err := NewDaytonaWorkspace(DaytonaConfig{
		BaseURL:     srv.URL,
		APIKey:      "key",
		WorkspaceID: "ws-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	err = ws.WriteFile(context.Background(), "/tmp/abs.txt", []byte("data"))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Absolute paths should be used as-is.
	if capturedPath != "/tmp/abs.txt" {
		t.Errorf("path = %q, want /tmp/abs.txt", capturedPath)
	}
}

func TestDaytonaReadFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/workspace/ws-1") && !strings.Contains(r.URL.Path, "toolbox"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/toolbox/files"):
			path := r.URL.Query().Get("path")
			if path != "/home/daytona/readme.md" {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("# Hello World"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ws, err := NewDaytonaWorkspace(DaytonaConfig{
		BaseURL:     srv.URL,
		APIKey:      "key",
		WorkspaceID: "ws-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	data, err := ws.ReadFile(context.Background(), "readme.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "# Hello World" {
		t.Errorf("content = %q, want %q", string(data), "# Hello World")
	}
}

func TestDaytonaReadFile_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && !strings.Contains(r.URL.Path, "toolbox"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/toolbox/files"):
			http.Error(w, "not found", http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ws, err := NewDaytonaWorkspace(DaytonaConfig{
		BaseURL:     srv.URL,
		APIKey:      "key",
		WorkspaceID: "ws-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, err = ws.ReadFile(context.Background(), "missing.txt")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestDaytonaListFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/workspace/ws-1") && !strings.Contains(r.URL.Path, "toolbox"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/toolbox/files/list"):
			files := []daytonaFileInfo{
				{Name: "main.go", Path: "/home/daytona/main.go", IsDir: false, Size: 1024},
				{Name: "pkg", Path: "/home/daytona/pkg", IsDir: true, Size: 0},
				{Name: "go.mod", Path: "/home/daytona/go.mod", IsDir: false, Size: 256},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(files)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ws, err := NewDaytonaWorkspace(DaytonaConfig{
		BaseURL:     srv.URL,
		APIKey:      "key",
		WorkspaceID: "ws-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	files, err := ws.ListFiles(context.Background(), ".")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3", len(files))
	}

	// Verify file parsing.
	found := make(map[string]FileInfo)
	for _, f := range files {
		found[f.Name] = f
	}

	if f, ok := found["main.go"]; !ok {
		t.Error("missing main.go")
	} else {
		if f.IsDir {
			t.Error("main.go should not be a directory")
		}
		if f.Size != 1024 {
			t.Errorf("main.go size = %d, want 1024", f.Size)
		}
	}

	if f, ok := found["pkg"]; !ok {
		t.Error("missing pkg")
	} else if !f.IsDir {
		t.Error("pkg should be a directory")
	}
}

func TestDaytonaRemoveFile(t *testing.T) {
	var deletedPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/workspace/") && !strings.Contains(r.URL.Path, "toolbox"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/toolbox/files"):
			deletedPath = r.URL.Query().Get("path")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ws, err := NewDaytonaWorkspace(DaytonaConfig{
		BaseURL:     srv.URL,
		APIKey:      "key",
		WorkspaceID: "ws-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	err = ws.RemoveFile(context.Background(), "trash.txt")
	if err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	if deletedPath != "/home/daytona/trash.txt" {
		t.Errorf("deleted path = %q, want /home/daytona/trash.txt", deletedPath)
	}
}

func TestDaytonaResolvePath(t *testing.T) {
	ws := &DaytonaWorkspace{baseURL: "http://x", workspaceID: "w"}

	tests := []struct {
		input string
		want  string
	}{
		{"file.txt", "/home/daytona/file.txt"},
		{"sub/dir/file.go", "/home/daytona/sub/dir/file.go"},
		{"/absolute/path", "/absolute/path"},
		{"/home/daytona/already", "/home/daytona/already"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := ws.resolvePath(tc.input)
			if got != tc.want {
				t.Errorf("resolvePath(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
