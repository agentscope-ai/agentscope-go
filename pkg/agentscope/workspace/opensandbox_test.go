package workspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenSandboxInterfaceCompliance(t *testing.T) {
	var _ Workspace = (*OpenSandboxWorkspace)(nil)
}

func TestNewOpenSandbox_MissingBaseURL(t *testing.T) {
	_, err := NewOpenSandboxWorkspace(OpenSandboxConfig{
		APIKey:    "key",
		SandboxID: "sb-1",
	})
	if err == nil {
		t.Fatal("expected error for missing base URL")
	}
	if !strings.Contains(err.Error(), "base URL") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewOpenSandbox_MissingAPIKey(t *testing.T) {
	_, err := NewOpenSandboxWorkspace(OpenSandboxConfig{
		BaseURL:   "http://localhost",
		SandboxID: "sb-1",
	})
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
	if !strings.Contains(err.Error(), "API key") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewOpenSandbox_WithExistingSandboxID(t *testing.T) {
	// When SandboxID is provided, no creation request should be made.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If any request is made, it means the implementation is calling the server
		// unexpectedly. For this test, we only expect no POST /sandboxes.
		if r.Method == http.MethodPost && r.URL.Path == "/sandboxes" {
			t.Error("unexpected sandbox creation request")
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ws, err := NewOpenSandboxWorkspace(OpenSandboxConfig{
		BaseURL:   srv.URL,
		APIKey:    "key",
		SandboxID: "existing-sb-42",
	})
	if err != nil {
		t.Fatalf("NewOpenSandboxWorkspace: %v", err)
	}
	if ws.BasePath() != "/home/user" {
		t.Errorf("BasePath = %q, want /home/user", ws.BasePath())
	}
}

func TestNewOpenSandbox_CreatesNewSandbox(t *testing.T) {
	var creationCalled bool
	var capturedTemplate string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/sandboxes" {
			creationCalled = true
			// Verify API key header.
			if r.Header.Get("X-API-Key") != "test-api-key" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			capturedTemplate, _ = body["template"].(string)

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"id": "new-sb-99"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ws, err := NewOpenSandboxWorkspace(OpenSandboxConfig{
		BaseURL:  srv.URL,
		APIKey:   "test-api-key",
		Template: "python3",
	})
	if err != nil {
		t.Fatalf("NewOpenSandboxWorkspace: %v", err)
	}
	if !creationCalled {
		t.Error("expected sandbox creation request")
	}
	if capturedTemplate != "python3" {
		t.Errorf("template = %q, want %q", capturedTemplate, "python3")
	}
	if ws.sandboxID != "new-sb-99" {
		t.Errorf("sandboxID = %q, want %q", ws.sandboxID, "new-sb-99")
	}
}

func TestNewOpenSandbox_DefaultTemplate(t *testing.T) {
	var capturedTemplate string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/sandboxes" {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			capturedTemplate, _ = body["template"].(string)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"id": "sb-new"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := NewOpenSandboxWorkspace(OpenSandboxConfig{
		BaseURL: srv.URL,
		APIKey:  "key",
		// No Template or SandboxID → should default to "default".
	})
	if err != nil {
		t.Fatalf("NewOpenSandboxWorkspace: %v", err)
	}
	if capturedTemplate != "default" {
		t.Errorf("template = %q, want %q", capturedTemplate, "default")
	}
}

func TestOpenSandboxExecute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/execute") {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)

			cmd, _ := body["command"].(string)
			if cmd != "ls -la" {
				t.Errorf("command = %q, want %q", cmd, "ls -la")
			}
			timeout, _ := body["timeout"].(float64)
			if timeout != 60 {
				t.Errorf("timeout = %v, want 60", timeout)
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"stdout":    "total 4\n-rw-r--r-- 1 user user 0 main.go\n",
				"stderr":    "",
				"exit_code": 0,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ws, err := NewOpenSandboxWorkspace(OpenSandboxConfig{
		BaseURL:   srv.URL,
		APIKey:    "key",
		SandboxID: "sb-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	result, err := ws.Execute(context.Background(), "ls -la")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "main.go") {
		t.Errorf("Stdout missing main.go: %q", result.Stdout)
	}
}

func TestOpenSandboxExecute_NonZeroExit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/execute") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"stdout":    "",
				"stderr":    "no such file",
				"exit_code": 2,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ws, err := NewOpenSandboxWorkspace(OpenSandboxConfig{
		BaseURL:   srv.URL,
		APIKey:    "key",
		SandboxID: "sb-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	result, err := ws.Execute(context.Background(), "cat missing")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2", result.ExitCode)
	}
	if result.Stderr != "no such file" {
		t.Errorf("Stderr = %q, want %q", result.Stderr, "no such file")
	}
}

func TestOpenSandboxWriteFile(t *testing.T) {
	var capturedPath string
	var capturedContent string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/files") {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			capturedPath, _ = body["path"].(string)
			capturedContent, _ = body["content"].(string)

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ws, err := NewOpenSandboxWorkspace(OpenSandboxConfig{
		BaseURL:   srv.URL,
		APIKey:    "key",
		SandboxID: "sb-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	data := []byte("package main\n\nfunc main() {}\n")
	err = ws.WriteFile(context.Background(), "/home/user/main.go", data)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if capturedPath != "/home/user/main.go" {
		t.Errorf("path = %q, want /home/user/main.go", capturedPath)
	}
	// Content should be base64 encoded.
	decoded, err := base64.StdEncoding.DecodeString(capturedContent)
	if err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Errorf("content mismatch: got %q", string(decoded))
	}
}

func TestOpenSandboxReadFile(t *testing.T) {
	fileContent := "hello from sandbox"
	encoded := base64.StdEncoding.EncodeToString([]byte(fileContent))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/files/") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"content": encoded})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ws, err := NewOpenSandboxWorkspace(OpenSandboxConfig{
		BaseURL:   srv.URL,
		APIKey:    "key",
		SandboxID: "sb-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	data, err := ws.ReadFile(context.Background(), "/home/user/test.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != fileContent {
		t.Errorf("content = %q, want %q", string(data), fileContent)
	}
}

func TestOpenSandboxListFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/files") && !strings.Contains(r.URL.Path, "/files/") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"entries": []any{
					map[string]any{"name": "main.go", "path": "/home/user/main.go", "is_dir": false, "size": 128.0},
					map[string]any{"name": "tests", "path": "/home/user/tests", "is_dir": true, "size": 0.0},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ws, err := NewOpenSandboxWorkspace(OpenSandboxConfig{
		BaseURL:   srv.URL,
		APIKey:    "key",
		SandboxID: "sb-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	files, err := ws.ListFiles(context.Background(), "/home/user")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}

	if files[0].Name != "main.go" || files[0].IsDir {
		t.Errorf("files[0] = %+v", files[0])
	}
	if files[0].Size != 128 {
		t.Errorf("files[0].Size = %d, want 128", files[0].Size)
	}
	if files[1].Name != "tests" || !files[1].IsDir {
		t.Errorf("files[1] = %+v", files[1])
	}
}

func TestOpenSandboxListFiles_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/files") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"entries": []any{}})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ws, err := NewOpenSandboxWorkspace(OpenSandboxConfig{
		BaseURL:   srv.URL,
		APIKey:    "key",
		SandboxID: "sb-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	files, err := ws.ListFiles(context.Background(), "/home/user")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("got %d files, want 0", len(files))
	}
}

func TestOpenSandboxRemoveFile(t *testing.T) {
	var deleteRequested bool
	var deletedPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/files/") {
			deleteRequested = true
			// Extract the encoded path after /files/.
			parts := strings.SplitN(r.URL.Path, "/files/", 2)
			if len(parts) == 2 {
				deletedPath = strings.ReplaceAll(parts[1], "%2F", "/")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ws, err := NewOpenSandboxWorkspace(OpenSandboxConfig{
		BaseURL:   srv.URL,
		APIKey:    "key",
		SandboxID: "sb-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	err = ws.RemoveFile(context.Background(), "/home/user/old.txt")
	if err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	if !deleteRequested {
		t.Error("expected DELETE request")
	}
	if deletedPath != "/home/user/old.txt" {
		t.Errorf("deleted path = %q, want /home/user/old.txt", deletedPath)
	}
}

func TestOpenSandboxClose(t *testing.T) {
	var closeCalled bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/sandboxes/sb-to-close" {
			closeCalled = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ws, err := NewOpenSandboxWorkspace(OpenSandboxConfig{
		BaseURL:   srv.URL,
		APIKey:    "key",
		SandboxID: "sb-to-close",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	err = ws.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closeCalled {
		t.Error("expected DELETE /sandboxes/{id} to be called")
	}
}

func TestOpenSandboxClose_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ws, err := NewOpenSandboxWorkspace(OpenSandboxConfig{
		BaseURL:   srv.URL,
		APIKey:    "key",
		SandboxID: "sb-1",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	err = ws.Close()
	if err == nil {
		t.Fatal("expected error from failed close")
	}
}
