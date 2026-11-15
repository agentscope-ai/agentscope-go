package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// This example demonstrates the permission system with different modes.
// It shows how permission rules control tool execution:
//   - ModeDefault: requires explicit allow rules or user confirmation
//   - ModeExplore: read-only mode that denies modifications
//   - ModeBypass: allows everything (for sandboxed environments)

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	readSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File path to read"}
		},
		"required": ["path"]
	}`)

	writeSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File path to write"},
			"content": {"type": "string", "description": "Content to write"}
		},
		"required": ["path", "content"]
	}`)

	readTool := tool.NewFunctionTool("read_file", "Read a file (read-only operation)", readSchema,
		func(ctx context.Context, input map[string]any) (any, error) {
			path, _ := input["path"].(string)
			return fmt.Sprintf("[simulated] Contents of %s: Hello, World!", path), nil
		},
	)

	writeTool := tool.NewFunctionTool("write_file", "Write content to a file", writeSchema,
		func(ctx context.Context, input map[string]any) (any, error) {
			path, _ := input["path"].(string)
			content, _ := input["content"].(string)
			return fmt.Sprintf("[simulated] Wrote %d bytes to %s", len(content), path), nil
		},
	)

	tk := tool.NewToolkit(readTool, writeTool)

	// --- Demo 1: Explore mode (read-only) ---
	fmt.Println("=== Demo 1: Explore Mode (read-only) ===")
	permCtx := permission.NewContext(permission.ModeExplore)
	engine := permission.NewEngine(permCtx)

	// In explore mode, read_file is allowed but write_file is denied.
	checkAndPrint(engine, readTool, map[string]any{"path": "/tmp/test.txt"})
	checkAndPrint(engine, writeTool, map[string]any{"path": "/tmp/test.txt", "content": "hello"})

	// --- Demo 2: Default mode with allow rules ---
	fmt.Println("\n=== Demo 2: Default Mode with Allow Rules ===")
	permCtx = permission.NewContext(permission.ModeDefault)
	engine = permission.NewEngine(permCtx)

	// Without rules, tools require confirmation (ASK).
	checkAndPrint(engine, readTool, map[string]any{"path": "/tmp/test.txt"})

	// Add an allow rule for read_file.
	engine.AddRule(permission.Rule{
		ToolName: "read_file",
		Behavior: permission.BehaviorAllow,
		Source:   "user",
	})
	checkAndPrint(engine, readTool, map[string]any{"path": "/tmp/test.txt"})

	// write_file still requires confirmation (no allow rule).
	checkAndPrint(engine, writeTool, map[string]any{"path": "/tmp/test.txt", "content": "hello"})

	// --- Demo 3: Bypass mode (sandboxed environment) ---
	fmt.Println("\n=== Demo 3: Bypass Mode (sandbox) ===")
	permCtx = permission.NewContext(permission.ModeBypass)
	engine = permission.NewEngine(permCtx)
	checkAndPrint(engine, readTool, map[string]any{"path": "/tmp/test.txt"})
	checkAndPrint(engine, writeTool, map[string]any{"path": "/tmp/test.txt", "content": "hello"})

	// --- Demo 4: Agent with permission context ---
	fmt.Println("\n=== Demo 4: Agent with Permission Context (Bypass) ===")
	permCtx = permission.NewContext(permission.ModeBypass)

	a := agent.NewUnifiedAgent(
		"safe-assistant",
		"You are an assistant. Use read_file to read files when asked.",
		cm,
		agent.WithToolkit(tk),
		agent.WithPermissionContext(permCtx),
		agent.WithReactConfig(agent.ReactConfig{MaxIters: 3}),
	)

	reply, err := a.Reply(context.Background(), "Read the file at /tmp/example.txt")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	if txt := reply.GetTextContent("\n"); txt != nil {
		fmt.Println("Assistant:", *txt)
	}
}

func checkAndPrint(engine *permission.Engine, t tool.Tool, input map[string]any) {
	decision, err := engine.CheckPermission(t, input)
	if err != nil {
		fmt.Printf("  %s: error: %v\n", t.Name(), err)
		return
	}
	fmt.Printf("  %s → %s: %s\n", t.Name(), decision.Behavior, decision.Message)
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
