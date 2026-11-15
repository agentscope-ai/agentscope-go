// Example mcp_streamable demonstrates the Streamable HTTP MCP client
// (pkg/agentscope/mcp/streamablehttp), backed by the official go-sdk.
//
// It is self-contained: an in-process, spec-compliant MCP server is started
// with the go-sdk, so no external service is needed. The agent reply at the
// end needs a chat model API key (ANTHROPIC_API_KEY, DASHSCOPE_API_KEY or
// OPENAI_API_KEY); without one, discovery and the direct tool call still run.
//
// Run:
//
//	go run ./examples/mcp_streamable/
//
// To connect to a real server, replace srv.URL with its endpoint and drop the
// in-process server, e.g. any go-sdk-based Streamable HTTP server (the
// in-process demo below is built the same way). Set credentials through
// streamablehttp.Config.Headers, and pin Config.ProtocolVersion only when
// debugging version negotiation.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/mcp"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/mcp/streamablehttp"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func main() {
	as.Init()

	srv := startDemoMCPServer()
	defer srv.Close()
	fmt.Printf("In-process MCP server running at %s\n\n", srv.URL)

	ctx := context.Background()

	// The ctx passed to NewClient governs the handshake only; the session
	// lives until Close. Close is idempotent, so a single defer is enough.
	client, err := streamablehttp.NewClient(ctx, &streamablehttp.Config{BaseURL: srv.URL})
	if err != nil {
		fmt.Println("MCP connect error:", err)
		return
	}
	defer client.Close()

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

	// Call a tool directly (no chat model required).
	resp, err := client.CallTool(ctx, "get_weather", map[string]any{"city": "Beijing"})
	if err != nil {
		fmt.Println("CallTool error:", err)
		return
	}
	for _, b := range resp.Content {
		if tb, ok := b.(message.TextBlock); ok {
			fmt.Println("Direct call result:", tb.Text)
		}
	}
	fmt.Println()

	// The same client backs an MCP toolkit for the agent.
	mcpToolkit, err := mcp.NewMCPToolkit(ctx, client)
	if err != nil {
		fmt.Println("NewMCPToolkit error:", err)
		return
	}

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("No chat model configured, skipping the agent reply:", err)
		return
	}
	a := agent.NewUnifiedAgent(
		"mcp-streamable-assistant",
		"You are a helpful assistant. Use get_weather to answer weather questions.",
		cm,
		agent.WithToolkit(mcpToolkit),
		agent.WithReactConfig(agent.ReactConfig{MaxIters: 5}),
	)
	reply, err := a.Reply(ctx, "What is the weather in Beijing?")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	if txt := reply.GetTextContent("\n"); txt != nil {
		fmt.Println("Assistant:", *txt)
	}
}

// startDemoMCPServer starts a spec-compliant Streamable HTTP server in-process
// using the go-sdk, with one demo tool.
func startDemoMCPServer() *httptest.Server {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "demo-mcp", Version: "1.0"}, nil)
	server.AddTool(&sdkmcp.Tool{
		Name:        "get_weather",
		Description: "Get current weather for a city",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{"type": "string", "description": "City name"},
			},
			"required": []string{"city"},
		},
	}, func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		var args struct {
			City string `json:"city"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "sunny, 25C in " + args.City}},
		}, nil
	})

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)
	return httptest.NewServer(handler)
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
