package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// DaytonaConfig configures a DaytonaWorkspace.
type DaytonaConfig struct {
	BaseURL        string
	APIKey         string
	SecretAPIKey   model.SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	WorkspaceID    string
	CommandTimeout time.Duration // default: 30s
}

// DaytonaWorkspace implements Workspace using the Daytona REST API.
type DaytonaWorkspace struct {
	baseURL     string
	apiKey      string
	workspaceID string
	httpClient  *http.Client
}

// Compile-time interface check.
var _ Workspace = (*DaytonaWorkspace)(nil)

// NewDaytonaWorkspace creates a Daytona workspace client and verifies the
// workspace exists by calling GET /workspace/{id}.
func NewDaytonaWorkspace(cfg DaytonaConfig) (*DaytonaWorkspace, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("workspace: daytona base URL is required")
	}
	apiKey := model.ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("workspace: daytona API key is required")
	}
	if cfg.WorkspaceID == "" {
		return nil, fmt.Errorf("workspace: daytona workspace ID is required")
	}

	timeout := cfg.CommandTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	w := &DaytonaWorkspace{
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:      apiKey,
		workspaceID: cfg.WorkspaceID,
		httpClient:  &http.Client{Timeout: timeout},
	}

	// Verify workspace exists.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/workspace/%s", w.baseURL, w.workspaceID), nil)
	if err != nil {
		return nil, fmt.Errorf("workspace: create request: %w", err)
	}
	w.setAuth(req)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("workspace: verify workspace: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("workspace: daytona workspace %q not found (status %d): %s",
			cfg.WorkspaceID, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return w, nil
}

// BasePath returns the default working directory in a Daytona workspace.
func (w *DaytonaWorkspace) BasePath() string { return "/home/daytona" }

// Execute runs a command in the Daytona workspace via the toolbox API.
func (w *DaytonaWorkspace) Execute(ctx context.Context, command string) (*ExecResult, error) {
	reqBody := daytonaExecRequest{Command: command}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("workspace: marshal exec request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/workspace/%s/toolbox/process/execute", w.baseURL, w.workspaceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("workspace: create request: %w", err)
	}
	w.setAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("workspace: execute command: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("workspace: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("workspace: execute failed (status %d): %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var execResp daytonaExecResponse
	if err := json.Unmarshal(respBody, &execResp); err != nil {
		return nil, fmt.Errorf("workspace: unmarshal exec response: %w", err)
	}

	return &ExecResult{
		Stdout:   execResp.Stdout,
		Stderr:   execResp.Stderr,
		ExitCode: execResp.ExitCode,
	}, nil
}

// WriteFile uploads file content to the Daytona workspace.
func (w *DaytonaWorkspace) WriteFile(ctx context.Context, path string, data []byte) error {
	fullPath := w.resolvePath(path)

	endpoint := fmt.Sprintf("%s/workspace/%s/toolbox/files?path=%s",
		w.baseURL, w.workspaceID, url.QueryEscape(fullPath))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("workspace: create request: %w", err)
	}
	w.setAuth(req)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("workspace: write file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("workspace: write file failed (status %d): %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

// ReadFile downloads file content from the Daytona workspace.
func (w *DaytonaWorkspace) ReadFile(ctx context.Context, path string) ([]byte, error) {
	fullPath := w.resolvePath(path)

	endpoint := fmt.Sprintf("%s/workspace/%s/toolbox/files?path=%s",
		w.baseURL, w.workspaceID, url.QueryEscape(fullPath))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("workspace: create request: %w", err)
	}
	w.setAuth(req)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("workspace: read file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("workspace: read file failed (status %d): %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return io.ReadAll(resp.Body)
}

// ListFiles lists files in a directory in the Daytona workspace.
func (w *DaytonaWorkspace) ListFiles(ctx context.Context, dir string) ([]FileInfo, error) {
	fullDir := w.resolvePath(dir)

	endpoint := fmt.Sprintf("%s/workspace/%s/toolbox/files/list?path=%s",
		w.baseURL, w.workspaceID, url.QueryEscape(fullDir))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("workspace: create request: %w", err)
	}
	w.setAuth(req)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("workspace: list files: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("workspace: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("workspace: list files failed (status %d): %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var daytonaFiles []daytonaFileInfo
	if err := json.Unmarshal(respBody, &daytonaFiles); err != nil {
		return nil, fmt.Errorf("workspace: unmarshal file list: %w", err)
	}

	files := make([]FileInfo, 0, len(daytonaFiles))
	for _, f := range daytonaFiles {
		files = append(files, FileInfo(f))
	}

	return files, nil
}

// RemoveFile deletes a file from the Daytona workspace.
func (w *DaytonaWorkspace) RemoveFile(ctx context.Context, path string) error {
	fullPath := w.resolvePath(path)

	endpoint := fmt.Sprintf("%s/workspace/%s/toolbox/files?path=%s",
		w.baseURL, w.workspaceID, url.QueryEscape(fullPath))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("workspace: create request: %w", err)
	}
	w.setAuth(req)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("workspace: remove file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("workspace: remove file failed (status %d): %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

// setAuth adds Bearer token authorization to a request.
func (w *DaytonaWorkspace) setAuth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+w.apiKey)
}

// resolvePath returns the full path for a given workspace-relative path.
// ExecPath implements ExecPathResolver: this backend's Execute runs the
// command with the caller's path resolved the same way ReadFile resolves it,
// which for this workspace means the absolute in-sandbox path. A relative path
// would be resolved by the shell against the image/sandbox working directory
// instead, i.e. against a different file.
func (w *DaytonaWorkspace) ExecPath(callerPath string) string { return w.resolvePath(callerPath) }

func (w *DaytonaWorkspace) resolvePath(path string) string {
	if strings.HasPrefix(path, "/") {
		return path
	}
	return "/home/daytona/" + path
}

// Internal types for Daytona API communication.

type daytonaExecRequest struct {
	Command string `json:"command"`
}

type daytonaExecResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

type daytonaFileInfo struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
}
