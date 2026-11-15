package mcp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// MCPTool wraps a single MCP server tool as a Go Tool interface implementation.
type MCPTool struct {
	tool.BaseTool
	client           Client
	toolName         string // original MCP tool name (may differ from adapted name)
	executionTimeout int    // seconds; 0 = no timeout
}

// NewMCPTool creates a Tool backed by an MCP client for the given tool schema.
func NewMCPTool(client Client, schema model.ToolSchema) *MCPTool {
	return &MCPTool{
		BaseTool: tool.BaseTool{
			ToolName:        schema.Function.Name,
			ToolDescription: schema.Function.Description,
			ToolSchema:      schema.Function.Parameters,
			ReadOnly:        false,
			ConcurrencySafe: true,
		},
		client:   client,
		toolName: schema.Function.Name,
	}
}

// Execute calls the MCP server to execute the tool.
func (t *MCPTool) Execute(ctx context.Context, args map[string]any) (*tool.ToolResponse, error) {
	if t.executionTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(t.executionTimeout)*time.Second)
		defer cancel()
	}
	return t.client.CallTool(ctx, t.toolName, args)
}

// NewMCPToolkit creates a Toolkit containing all tools from an MCP client.
// Use opts to filter tools by enable/disable lists.
func NewMCPToolkit(ctx context.Context, client Client, opts ...MCPToolkitOption) (*tool.Toolkit, error) {
	schemas, err := client.ListTools(ctx)
	if err != nil {
		return nil, err
	}

	cfg := mcpToolkitConfig{}
	for _, o := range opts {
		o(&cfg)
	}

	var tools []tool.Tool
	for _, s := range schemas {
		if !cfg.isAllowed(s.Function.Name) {
			continue
		}
		t := NewMCPTool(client, s)
		if cfg.executionTimeout > 0 {
			t.executionTimeout = cfg.executionTimeout
		}
		tools = append(tools, t)
	}

	return tool.NewToolkit(tools...), nil
}

type mcpToolkitConfig struct {
	enableTools      map[string]bool
	disableTools     map[string]bool
	executionTimeout int // seconds; 0 = no timeout
}

func (c *mcpToolkitConfig) isAllowed(name string) bool {
	if len(c.disableTools) > 0 && c.disableTools[name] {
		return false
	}
	if len(c.enableTools) > 0 {
		return c.enableTools[name]
	}
	return true
}

// MCPToolkitOption configures NewMCPToolkit.
type MCPToolkitOption func(*mcpToolkitConfig)

// WithEnableTools whitelists specific tools; others are excluded.
func WithEnableTools(names ...string) MCPToolkitOption {
	return func(c *mcpToolkitConfig) {
		c.enableTools = make(map[string]bool, len(names))
		for _, n := range names {
			c.enableTools[n] = true
		}
	}
}

// WithDisableTools blacklists specific tools.
func WithDisableTools(names ...string) MCPToolkitOption {
	return func(c *mcpToolkitConfig) {
		c.disableTools = make(map[string]bool, len(names))
		for _, n := range names {
			c.disableTools[n] = true
		}
	}
}

// WithExecutionTimeout sets a per-tool call timeout in seconds for all tools
// created by this toolkit.
func WithExecutionTimeout(seconds int) MCPToolkitOption {
	return func(c *mcpToolkitConfig) {
		c.executionTimeout = seconds
	}
}

// MergeToolkits combines multiple toolkits into one.
func MergeToolkits(toolkits ...*tool.Toolkit) *tool.Toolkit {
	var allTools []tool.Tool
	for _, tk := range toolkits {
		for _, schema := range tk.GetToolSchemas() {
			t := tk.Get(schema.Function.Name)
			if t != nil {
				allTools = append(allTools, t)
			}
		}
	}
	return tool.NewToolkit(allTools...)
}

// Compile-time interface checks.
var _ Client = (*StdioClient)(nil)
var _ Client = (*HttpClient)(nil)
var _ tool.Tool = (*MCPTool)(nil)

// ensure BaseTool's ToolSchema field is json.RawMessage
var _ json.RawMessage = json.RawMessage(nil)
