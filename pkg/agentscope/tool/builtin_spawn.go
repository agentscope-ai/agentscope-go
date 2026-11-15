package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

var spawnSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"prompt": {
			"type": "string",
			"description": "The task for the subagent to perform"
		},
		"description": {
			"type": "string",
			"description": "A short (3-5 word) description of the task"
		},
		"system_prompt": {
			"type": "string",
			"description": "Optional system prompt for the subagent"
		}
	},
	"required": ["prompt"]
}`)

// Spawner is the interface used by the spawn tool to create subagents.
// Implemented by *agent.UnifiedAgent via its Spawn method.
type Spawner interface {
	Spawn(ctx context.Context, cfg *SpawnConfig, task string) (*SpawnResult, error)
}

// SpawnConfig mirrors agent.SpawnConfig for the tool layer.
type SpawnConfig struct {
	Name         string
	SystemPrompt string
	Timeout      time.Duration
}

// SpawnResult mirrors agent.SpawnResult for the tool layer.
type SpawnResult struct {
	Output    string
	TokensIn  int
	TokensOut int
	Duration  time.Duration
}

type spawnTool struct {
	BaseTool
	spawner Spawner
}

// SpawnOption configures the spawn tool.
type SpawnOption func(*spawnTool)

// WithSpawner sets the Spawner implementation.
func WithSpawner(s Spawner) SpawnOption {
	return func(t *spawnTool) { t.spawner = s }
}

func (t *spawnTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	prompt, _ := args["prompt"].(string)
	if prompt == "" {
		return NewErrorResponse(fmt.Errorf("prompt is required")), nil
	}

	if t.spawner == nil {
		return NewErrorResponse(fmt.Errorf("no spawner configured")), nil
	}

	systemPrompt, _ := args["system_prompt"].(string)

	cfg := &SpawnConfig{
		SystemPrompt: systemPrompt,
	}

	result, err := t.spawner.Spawn(ctx, cfg, prompt)
	if err != nil {
		return NewErrorResponse(err), nil
	}

	out := map[string]any{
		"output":      result.Output,
		"tokens_in":   result.TokensIn,
		"tokens_out":  result.TokensOut,
		"duration_ms": result.Duration.Milliseconds(),
	}
	b, _ := json.Marshal(out)
	return NewTextResponse(string(b)), nil
}

// SpawnTool returns a tool that spawns subagents to handle tasks.
func SpawnTool(opts ...SpawnOption) Tool {
	t := &spawnTool{
		BaseTool: BaseTool{
			ToolName:        "Agent",
			ToolDescription: "Launch a subagent to handle a complex task. The subagent runs with its own context and returns the result.",
			ToolSchema:      spawnSchema,
		},
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}
