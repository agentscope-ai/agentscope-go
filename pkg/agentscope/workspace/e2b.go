package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

const e2bBaseURL = "https://api.e2b.dev"

// E2BConfig configures an E2B cloud sandbox workspace.
type E2BConfig struct {
	APIKey       string
	SecretAPIKey model.SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	Template     string          // sandbox template ID (default: "base")
	Timeout      time.Duration
}

// E2BWorkspace provides file and command operations in an E2B cloud sandbox.
type E2BWorkspace struct {
	apiKey    string
	sandboxID string
	client    *http.Client
}

// NewE2BWorkspace creates a new E2B sandbox.
func NewE2BWorkspace(ctx context.Context, cfg E2BConfig) (*E2BWorkspace, error) {
	apiKey := model.ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("e2b: api key is required")
	}
	template := cfg.Template
	if template == "" {
		template = "base"
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	w := &E2BWorkspace{
		apiKey: apiKey,
		client: &http.Client{Timeout: timeout},
	}

	// Create sandbox
	body := map[string]any{"templateID": template}
	resp, err := w.doAPI(ctx, "POST", "/sandboxes", body)
	if err != nil {
		return nil, fmt.Errorf("e2b: create sandbox: %w", err)
	}
	if id, ok := resp["sandboxID"].(string); ok {
		w.sandboxID = id
	} else {
		return nil, fmt.Errorf("e2b: no sandbox ID in response")
	}

	return w, nil
}

func (w *E2BWorkspace) WriteFile(ctx context.Context, path string, data []byte) error {
	_, err := w.doAPI(ctx, "POST", fmt.Sprintf("/sandboxes/%s/files", w.sandboxID), map[string]any{
		"path":    path,
		"content": string(data),
	})
	return err
}

func (w *E2BWorkspace) ReadFile(ctx context.Context, path string) ([]byte, error) {
	resp, err := w.doAPI(ctx, "GET", fmt.Sprintf("/sandboxes/%s/files?path=%s", w.sandboxID, path), nil)
	if err != nil {
		return nil, err
	}
	content, _ := resp["content"].(string)
	return []byte(content), nil
}

func (w *E2BWorkspace) ListFiles(ctx context.Context, dir string) ([]FileInfo, error) {
	resp, err := w.doAPI(ctx, "GET", fmt.Sprintf("/sandboxes/%s/files?path=%s&list=true", w.sandboxID, dir), nil)
	if err != nil {
		return nil, err
	}
	var files []FileInfo
	if entries, ok := resp["entries"].([]any); ok {
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			name, _ := em["name"].(string)
			isDir, _ := em["isDir"].(bool)
			size, _ := em["size"].(float64)
			files = append(files, FileInfo{
				Name:  name,
				Path:  dir + "/" + name,
				IsDir: isDir,
				Size:  int64(size),
			})
		}
	}
	return files, nil
}

func (w *E2BWorkspace) RemoveFile(ctx context.Context, path string) error {
	_, err := w.doAPI(ctx, "DELETE", fmt.Sprintf("/sandboxes/%s/files?path=%s", w.sandboxID, path), nil)
	return err
}

func (w *E2BWorkspace) Execute(ctx context.Context, command string) (*ExecResult, error) {
	resp, err := w.doAPI(ctx, "POST", fmt.Sprintf("/sandboxes/%s/commands", w.sandboxID), map[string]any{
		"cmd": command,
	})
	if err != nil {
		return nil, err
	}
	stdout, _ := resp["stdout"].(string)
	stderr, _ := resp["stderr"].(string)
	exitCode := 0
	if v, ok := resp["exitCode"].(float64); ok {
		exitCode = int(v)
	}
	return &ExecResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCode}, nil
}

func (w *E2BWorkspace) BasePath() string { return "/home/user" }

// Close destroys the E2B sandbox.
func (w *E2BWorkspace) Close(ctx context.Context) error {
	_, err := w.doAPI(ctx, "DELETE", fmt.Sprintf("/sandboxes/%s", w.sandboxID), nil)
	return err
}

func (w *E2BWorkspace) doAPI(ctx context.Context, method, path string, body map[string]any) (map[string]any, error) {
	url := strings.TrimRight(e2bBaseURL, "/") + path

	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-E2B-Api-Key", w.apiKey)
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
		return nil, fmt.Errorf("e2b: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result map[string]any
	if len(respBody) > 0 {
		_ = json.Unmarshal(respBody, &result)
	}
	return result, nil
}
