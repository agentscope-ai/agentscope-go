package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// HttpClient communicates with an MCP server via HTTP JSON-RPC.
type HttpClient struct {
	url     string
	headers map[string]string
	client  *http.Client
	nextID  int
	mu      sync.Mutex
}

// NewHttpClient creates an HTTP-based MCP client and initializes the session.
func NewHttpClient(ctx context.Context, cfg *HttpConfig) (*HttpClient, error) {
	timeout := 30 * time.Second
	if cfg.Timeout > 0 {
		timeout = time.Duration(cfg.Timeout) * time.Second
	}

	c := &HttpClient{
		url:     cfg.URL,
		headers: cfg.Headers,
		client:  &http.Client{Timeout: timeout},
		nextID:  1,
	}

	if err := c.initialize(ctx); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *HttpClient) initialize(ctx context.Context) error {
	_, err := c.call(ctx, "initialize", initializeParams{
		ProtocolVersion: "2024-11-05",
		Capabilities:    map[string]any{},
		ClientInfo:      clientInfo{Name: "agentscope-go", Version: "2.0"},
	})
	return err
}

func (c *HttpClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	c.mu.Unlock()

	req := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}

	httpResp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("mcp http: request: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("mcp http: read response: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcp http: status %d: %s", httpResp.StatusCode, string(respBody))
	}

	var resp jsonrpcResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("mcp http: parse response: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("mcp error %d: %s", resp.Error.Code, resp.Error.Message)
	}

	return resp.Result, nil
}

// ListTools queries the MCP server for available tools.
func (c *HttpClient) ListTools(ctx context.Context) ([]model.ToolSchema, error) {
	result, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}

	var toolList toolListResult
	if err := json.Unmarshal(result, &toolList); err != nil {
		return nil, fmt.Errorf("mcp http: parse tools: %w", err)
	}

	return convertToolSchemas(toolList.Tools), nil
}

// CallTool invokes a tool on the MCP server.
func (c *HttpClient) CallTool(ctx context.Context, name string, input map[string]any) (*tool.ToolResponse, error) {
	result, err := c.call(ctx, "tools/call", toolCallParams{
		Name:      name,
		Arguments: input,
	})
	if err != nil {
		return nil, err
	}

	var callResult toolCallResult
	if err := json.Unmarshal(result, &callResult); err != nil {
		return nil, fmt.Errorf("mcp http: parse result: %w", err)
	}

	return convertToolResult(callResult), nil
}

// Close is a no-op for HTTP clients (no persistent connection).
func (c *HttpClient) Close() error {
	return nil
}
