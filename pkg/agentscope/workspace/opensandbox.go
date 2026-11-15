package workspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// OpenSandboxConfig configures an OpenSandbox workspace.
type OpenSandboxConfig struct {
	BaseURL        string
	APIKey         string
	SecretAPIKey   model.SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	SandboxID      string          // if empty, a new sandbox is created
	Template       string          // sandbox template (e.g. "default")
	CommandTimeout time.Duration   // default: 60s
}

// OpenSandboxWorkspace provides file and command operations via an OpenSandbox REST API.
type OpenSandboxWorkspace struct {
	baseURL        string
	apiKey         string
	sandboxID      string
	commandTimeout time.Duration
	client         *http.Client
}

// Compile-time interface check.
var _ Workspace = (*OpenSandboxWorkspace)(nil)

// NewOpenSandboxWorkspace creates or connects to an OpenSandbox instance.
func NewOpenSandboxWorkspace(cfg OpenSandboxConfig) (*OpenSandboxWorkspace, error) { //nolint:gocritic // stable API: value receiver for backward compat
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("opensandbox: base URL is required")
	}
	apiKey := model.ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("opensandbox: API key is required")
	}

	timeout := cfg.CommandTimeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	w := &OpenSandboxWorkspace{
		baseURL:        strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:         apiKey,
		commandTimeout: timeout,
		client:         &http.Client{Timeout: 5 * time.Minute},
	}

	if cfg.SandboxID != "" {
		w.sandboxID = cfg.SandboxID
	} else {
		// Create a new sandbox.
		template := cfg.Template
		if template == "" {
			template = "default"
		}
		body := map[string]any{"template": template}
		resp, err := w.doRequest(context.Background(), "POST", "/sandboxes", body)
		if err != nil {
			return nil, fmt.Errorf("opensandbox: create sandbox: %w", err)
		}
		id, ok := resp["id"].(string)
		if !ok {
			if id, ok = resp["sandbox_id"].(string); !ok {
				return nil, fmt.Errorf("opensandbox: no sandbox ID in create response")
			}
		}
		w.sandboxID = id
	}

	return w, nil
}

// BasePath returns the default working directory inside the sandbox.
func (w *OpenSandboxWorkspace) BasePath() string { return "/home/user" }

// Execute runs a command inside the sandbox.
func (w *OpenSandboxWorkspace) Execute(ctx context.Context, command string) (*ExecResult, error) {
	timeoutSec := int(w.commandTimeout.Seconds())
	body := map[string]any{
		"command": command,
		"timeout": timeoutSec,
	}
	resp, err := w.doRequest(ctx, "POST", fmt.Sprintf("/sandboxes/%s/execute", w.sandboxID), body)
	if err != nil {
		return nil, fmt.Errorf("opensandbox: execute: %w", err)
	}

	stdout, _ := resp["stdout"].(string)
	stderr, _ := resp["stderr"].(string)
	exitCode := 0
	if v, ok := resp["exit_code"].(float64); ok {
		exitCode = int(v)
	}
	return &ExecResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCode}, nil
}

// WriteFile writes a file to the sandbox filesystem.
func (w *OpenSandboxWorkspace) WriteFile(ctx context.Context, path string, data []byte) error {
	body := map[string]any{
		"path":    path,
		"content": base64.StdEncoding.EncodeToString(data),
	}
	_, err := w.doRequest(ctx, "POST", fmt.Sprintf("/sandboxes/%s/files", w.sandboxID), body)
	if err != nil {
		return fmt.Errorf("opensandbox: write file: %w", err)
	}
	return nil
}

// ReadFile reads a file from the sandbox filesystem.
func (w *OpenSandboxWorkspace) ReadFile(ctx context.Context, path string) ([]byte, error) {
	encodedPath := strings.ReplaceAll(path, "/", "%2F")
	resp, err := w.doRequest(ctx, "GET", fmt.Sprintf("/sandboxes/%s/files/%s", w.sandboxID, encodedPath), nil)
	if err != nil {
		return nil, fmt.Errorf("opensandbox: read file: %w", err)
	}

	if encoded, ok := resp["content"].(string); ok {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			// Content might be plain text.
			return []byte(encoded), nil
		}
		return decoded, nil
	}
	return nil, fmt.Errorf("opensandbox: no content in read response")
}

// ListFiles lists files in a directory inside the sandbox.
func (w *OpenSandboxWorkspace) ListFiles(ctx context.Context, dir string) ([]FileInfo, error) {
	resp, err := w.doRequest(ctx, "GET", fmt.Sprintf("/sandboxes/%s/files?path=%s", w.sandboxID, dir), nil)
	if err != nil {
		return nil, fmt.Errorf("opensandbox: list files: %w", err)
	}

	var files []FileInfo
	entries, ok := resp["entries"].([]any)
	if !ok {
		return files, nil
	}
	for _, e := range entries {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		name, _ := em["name"].(string)
		entryPath, _ := em["path"].(string)
		isDir, _ := em["is_dir"].(bool)
		size, _ := em["size"].(float64)
		if entryPath == "" {
			entryPath = dir + "/" + name
		}
		files = append(files, FileInfo{
			Name:  name,
			Path:  entryPath,
			IsDir: isDir,
			Size:  int64(size),
		})
	}
	return files, nil
}

// RemoveFile deletes a file from the sandbox filesystem.
func (w *OpenSandboxWorkspace) RemoveFile(ctx context.Context, path string) error {
	encodedPath := strings.ReplaceAll(path, "/", "%2F")
	_, err := w.doRequest(ctx, "DELETE", fmt.Sprintf("/sandboxes/%s/files/%s", w.sandboxID, encodedPath), nil)
	if err != nil {
		return fmt.Errorf("opensandbox: remove file: %w", err)
	}
	return nil
}

// Close destroys the sandbox, freeing remote resources.
func (w *OpenSandboxWorkspace) Close() error {
	_, err := w.doRequest(context.Background(), "DELETE", fmt.Sprintf("/sandboxes/%s", w.sandboxID), nil)
	if err != nil {
		return fmt.Errorf("opensandbox: close: %w", err)
	}
	return nil
}

// doRequest performs an HTTP request against the OpenSandbox API.
func (w *OpenSandboxWorkspace) doRequest(ctx context.Context, method, path string, body map[string]any) (map[string]any, error) {
	url := w.baseURL + path

	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("opensandbox: marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("opensandbox: create request: %w", err)
	}
	req.Header.Set("X-API-Key", w.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("opensandbox: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result map[string]any
	if len(respBody) > 0 {
		_ = json.Unmarshal(respBody, &result)
	}
	return result, nil
}
