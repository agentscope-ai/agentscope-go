package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/mcp"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// This example demonstrates MCP (Model Context Protocol) integration.
// It starts a mock MCP HTTP server, connects to it via the HTTP client,
// discovers tools, and uses them with a UnifiedAgent.
//
// For real usage, replace the mock server with a real MCP server:
//   - StdioClient: spawns a subprocess (e.g., npx @modelcontextprotocol/server-filesystem)
//   - HttpClient: connects to an HTTP MCP server

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	// Start a mock MCP server for demonstration.
	srv := startMockMCPServer()
	defer srv.Close()
	fmt.Printf("Mock MCP server running at %s\n\n", srv.Addr)

	ctx := context.Background()

	// Connect to the MCP server via HTTP.
	client, err := mcp.NewHttpClient(ctx, &mcp.HttpConfig{
		URL: fmt.Sprintf("http://localhost%s", srv.Addr),
	})
	if err != nil {
		fmt.Println("MCP connect error:", err)
		return
	}
	defer client.Close()

	// Discover tools from the MCP server.
	schemas, err := client.ListTools(ctx)
	if err != nil {
		fmt.Println("ListTools error:", err)
		return
	}

	fmt.Printf("Discovered %d MCP tool(s):\n", len(schemas))
	for _, s := range schemas {
		fmt.Printf("  - %s: %s\n", s.Function.Name, s.Function.Description)
	}
	fmt.Println()

	// Create a toolkit from MCP tools.
	mcpToolkit, err := mcp.NewMCPToolkit(ctx, client)
	if err != nil {
		fmt.Println("NewMCPToolkit error:", err)
		return
	}

	// Optionally merge with local tools.
	localTool := tool.NewFunctionTool("get_time", "Get the current time", nil,
		func(ctx context.Context, input map[string]any) (any, error) {
			return "2026-06-24T10:30:00Z", nil
		},
	)
	merged := mcp.MergeToolkits(mcpToolkit, tool.NewToolkit(localTool))

	// Create an agent with the merged toolkit.
	a := agent.NewUnifiedAgent(
		"mcp-assistant",
		"You are a helpful assistant with access to MCP tools and local tools. "+
			"Use list_directory to list files, and get_time to get the current time.",
		cm,
		agent.WithToolkit(merged),
		agent.WithReactConfig(agent.ReactConfig{MaxIters: 5}),
	)

	reply, err := a.Reply(ctx, "List the files in the /home directory.")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	if txt := reply.GetTextContent("\n"); txt != nil {
		fmt.Println("Assistant:", *txt)
	}
}

// startMockMCPServer runs a minimal JSON-RPC MCP server for demo purposes.
func startMockMCPServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int             `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": "mock-mcp", "version": "1.0"},
			}
		case "tools/list":
			result = map[string]any{
				"tools": []map[string]any{
					{
						"name":        "list_directory",
						"description": "List files in a directory",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"path": map[string]any{"type": "string", "description": "Directory path"},
							},
							"required": []string{"path"},
						},
					},
				},
			}
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			json.Unmarshal(req.Params, &params)

			path, _ := params.Arguments["path"].(string)
			result = map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": fmt.Sprintf("Files in %s:\n  documents/\n  photos/\n  notes.txt", path)},
				},
			}
		default:
			result = map[string]any{}
		}

		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	srv := &http.Server{Addr: ":18930", Handler: mux}
	go srv.ListenAndServe()
	return srv
}

func loadChatModelFromEnv() (model.ChatModel, error) {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return model.NewAnthropicChatModel(&model.AnthropicConfig{
			APIKey:          key,
			Model:           "claude-sonnet-4-20250514",
			MaxOutputTokens: 1024,
		})
	}
	if key := os.Getenv("DASHSCOPE_API_KEY"); key != "" {
		return model.NewDashScopeChatModel(model.DashScopeConfig{
			APIKey:  key,
			BaseURL: os.Getenv("DASHSCOPE_BASE_URL"),
			Model:   "qwen-plus",
		})
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		return model.NewOpenAIChatModel(model.OpenAIConfig{
			APIKey: key,
			Model:  "gpt-4o-mini",
		})
	}
	return nil, fmt.Errorf("set ANTHROPIC_API_KEY, DASHSCOPE_API_KEY, or OPENAI_API_KEY")
}
