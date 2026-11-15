package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// GatewayClient is the host-side facade over the in-workspace MCP gateway.
// It communicates with a gateway process running inside Docker/E2B containers
// to manage MCP servers and execute tools remotely.
type GatewayClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewGatewayClient creates a client for the in-workspace MCP gateway.
func NewGatewayClient(baseURL, token string) *GatewayClient {
	return &GatewayClient{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// Health checks if the gateway is reachable.
func (c *GatewayClient) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	c.setAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("gateway health check failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway unhealthy: status %d", resp.StatusCode)
	}
	return nil
}

// ListMCPs lists the MCP servers registered with the gateway.
func (c *GatewayClient) ListMCPs(ctx context.Context) ([]GatewayMCPInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/mcps", nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result []GatewayMCPInfo
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode MCP list: %w", err)
	}
	return result, nil
}

// ConnectMCP registers an MCP server with the gateway.
func (c *GatewayClient) ConnectMCP(ctx context.Context, config *GatewayMCPConfig) error {
	body, _ := json.Marshal(config)
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/mcps", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("connect MCP failed: status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// DisconnectMCP removes an MCP server from the gateway.
func (c *GatewayClient) DisconnectMCP(ctx context.Context, name string) error {
	req, err := http.NewRequestWithContext(ctx, "DELETE", c.baseURL+"/mcps/"+name, nil)
	if err != nil {
		return err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("disconnect MCP %q: status %d", name, resp.StatusCode)
	}
	return nil
}

// CallTool executes a tool through the gateway.
func (c *GatewayClient) CallTool(ctx context.Context, mcpName, toolName string, input map[string]any) (*tool.ToolResponse, error) {
	body, _ := json.Marshal(input)
	url := fmt.Sprintf("%s/mcps/%s/tools/%s", c.baseURL, mcpName, toolName)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Content string `json:"content"`
		IsError bool   `json:"is_error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode tool response: %w", err)
	}

	state := message.ToolResultSuccess
	if result.IsError {
		state = message.ToolResultError
	}
	return &tool.ToolResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: result.Content}},
		State:   state,
	}, nil
}

func (c *GatewayClient) setAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// GatewayMCPInfo describes an MCP server known to the gateway.
type GatewayMCPInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// GatewayMCPConfig configures an MCP server for the gateway.
type GatewayMCPConfig struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"`
}

// GatewayMCPTool is a tool backed by a remote MCP server via the gateway.
// It implements the Tool interface, forwarding Execute calls through HTTP.
type GatewayMCPTool struct {
	tool.BaseTool
	mcpName string
	gateway *GatewayClient
}

// NewGatewayMCPTool creates a gateway-backed tool.
func NewGatewayMCPTool(mcpName, toolName, description string, schema json.RawMessage, gateway *GatewayClient) *GatewayMCPTool {
	return &GatewayMCPTool{
		BaseTool: tool.BaseTool{
			ToolName:        fmt.Sprintf("mcp__%s__%s", mcpName, toolName),
			ToolDescription: description,
			ToolSchema:      schema,
			ConcurrencySafe: true,
		},
		mcpName: mcpName,
		gateway: gateway,
	}
}

func (t *GatewayMCPTool) Execute(ctx context.Context, input map[string]any) (*tool.ToolResponse, error) {
	rawName := t.ToolName
	// Strip the mcp__<name>__ prefix to get the upstream tool name
	if len(t.mcpName) > 0 {
		prefix := "mcp__" + t.mcpName + "__"
		if len(rawName) > len(prefix) {
			rawName = rawName[len(prefix):]
		}
	}
	return t.gateway.CallTool(ctx, t.mcpName, rawName, input)
}

func (t *GatewayMCPTool) CheckPermissions(_ map[string]any) (permission.Decision, error) {
	return permission.Decision{Behavior: permission.BehaviorPassthrough}, nil
}

func (t *GatewayMCPTool) MatchRule(_ string, _ map[string]any) bool { return false }

func (t *GatewayMCPTool) GenerateSuggestions(_ map[string]any) []permission.Rule { return nil }
