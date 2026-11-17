// Package main demonstrates serving an AgentScope agent from the Gin and Echo
// web frameworks: chat replies as Server-Sent Events (SSE), plus the agent's
// tools exposed through MCP HTTP endpoints — all in one process.
//
// AgentScope service and MCP handlers are plain net/http handlers, so any
// framework can host them: the streaming endpoint uses a native framework
// handler, while the MCP handler is mounted with gin.WrapH / echo.WrapHandler.
//
// Start the servers:
//
//	DASHSCOPE_API_KEY=... go run ./examples/framework_service
//
// Then interact via curl (Gin on :8081, Echo on :8082):
//
//	# SSE chat stream (same route on both frameworks)
//	curl -N "http://localhost:8081/chat/stream?message=weather%20in%20Tokyo"
//	curl -N "http://localhost:8082/chat/stream?message=weather%20in%20Tokyo"
//
//	# MCP tool endpoints
//	curl http://localhost:8081/mcp/health
//	curl http://localhost:8081/mcp/tools
//	curl -X POST http://localhost:8081/mcp/tools/get_weather \
//	  -H 'Content-Type: application/json' -d '{"location":"Tokyo"}'
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labstack/echo/v4"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/mcp"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/service"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

const (
	ginAddr  = ":8081"
	echoAddr = ":8082"
)

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	weatherTool := newWeatherTool()
	chatAgent := newAgent(cm, weatherTool)
	mcpHandler := newMCPHandler(weatherTool)

	ginServer := &http.Server{
		Addr:              ginAddr,
		Handler:           newGinEngine(chatAgent, mcpHandler),
		ReadHeaderTimeout: 10 * time.Second,
	}
	echoRouter := newEchoRouter(chatAgent, mcpHandler)
	echoRouter.Server.ReadHeaderTimeout = 10 * time.Second

	errCh := make(chan error, 2)
	go func() { errCh <- ginServer.ListenAndServe() }()
	go func() { errCh <- echoRouter.Start(echoAddr) }()

	log.Printf("Gin  server on %s — curl -N 'http://localhost%s/chat/stream?message=weather%%20in%%20Tokyo'", ginAddr, ginAddr)
	log.Printf("Echo server on %s — same routes; MCP tools under /mcp on both", echoAddr)
	log.Fatal(<-errCh)
}

// newAgent builds the chat agent served by both frameworks.
func newAgent(cm model.ChatModel, weatherTool tool.Tool) *agent.UnifiedAgent {
	return agent.NewUnifiedAgent(
		"weather-bot",
		"You are a concise weather assistant.",
		cm,
		agent.WithToolkit(tool.NewToolkit(weatherTool)),
		agent.WithReactConfig(agent.ReactConfig{MaxIters: 5}),
	)
}

// newMCPHandler exposes the given tools under /mcp (GET /mcp/health,
// GET /mcp/tools, POST /mcp/tools/{name}).
func newMCPHandler(tools ...tool.Tool) http.Handler {
	srv := mcp.NewServer(tools, mcp.ServerConfig{})
	return http.StripPrefix("/mcp", srv.Handler())
}

// streamChat writes the agent reply as SSE events until the reply completes
// or the client disconnects (the request context is canceled).
func streamChat(w http.ResponseWriter, r *http.Request, a *agent.UnifiedAgent, msg string) {
	sse, err := service.NewSSEWriter(w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	events, err := a.ReplyStream(r.Context(), msg)
	if err != nil {
		_ = sse.WriteEvent("error", map[string]string{"error": err.Error()})
		return
	}
	for evt := range events {
		// WriteEvent only reports marshal failures; a disconnected client
		// surfaces via the canceled request context closing the channel.
		if err := sse.WriteEvent(string(evt.GetEventType()), evt); err != nil {
			return
		}
	}
}

// newGinEngine mounts the chat stream and MCP routes on a Gin engine.
func newGinEngine(a *agent.UnifiedAgent, mcpHandler http.Handler) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/chat/stream", func(c *gin.Context) {
		msg := c.Query("message")
		if msg == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "query parameter 'message' is required"})
			return
		}
		streamChat(c.Writer, c.Request, a, msg)
	})
	r.Any("/mcp/*path", gin.WrapH(mcpHandler))
	return r
}

// newEchoRouter mounts the same routes on an Echo router.
func newEchoRouter(a *agent.UnifiedAgent, mcpHandler http.Handler) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.GET("/chat/stream", func(c echo.Context) error {
		msg := c.QueryParam("message")
		if msg == "" {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "query parameter 'message' is required"})
		}
		streamChat(c.Response().Writer, c.Request(), a, msg)
		return nil
	})
	e.Any("/mcp/*", echo.WrapHandler(mcpHandler))
	return e
}

func newWeatherTool() tool.Tool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"location": {"type": "string", "description": "City name"}
		},
		"required": ["location"]
	}`)
	return tool.NewFunctionTool("get_weather", "Get weather for a city", schema,
		func(_ context.Context, input map[string]any) (any, error) {
			loc, _ := input["location"].(string)
			return map[string]any{"location": loc, "temp": "22°C", "condition": "sunny"}, nil
		},
	)
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
