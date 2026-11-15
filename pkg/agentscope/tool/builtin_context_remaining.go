package tool

import (
	"context"
	"encoding/json"
)

// ContextRemainingFunc reports the tokens left in the agent's context window.
// ok=false means the estimate is unavailable (no window size known).
// agent.WithContextRemainingTool wires it to the agent's own accounting.
type ContextRemainingFunc func(ctx context.Context) (tokensLeft int64, ok bool)

type contextRemainingTool struct {
	BaseTool
	remaining ContextRemainingFunc
}

// NewContextRemainingTool creates the get_context_remaining tool (codex
// parity): the model can check its budget before starting an expensive
// subtask, and can decide to compress early. Panics on a nil func, mirroring
// NewCompressContextTool. Concurrency-safe: the callback only reads state.
func NewContextRemainingTool(remaining ContextRemainingFunc) Tool {
	if remaining == nil {
		panic("tool: NewContextRemainingTool requires a non-nil remaining func")
	}
	return &contextRemainingTool{
		BaseTool: BaseTool{
			ToolName:        "get_context_remaining",
			ToolDescription: "Get the remaining tokens in the current context window. Use it to decide whether to wrap up, compress context, or start a large subtask.",
			ToolSchema:      json.RawMessage(`{"type": "object", "properties": {}, "additionalProperties": false}`),
			ReadOnly:        true,
			ConcurrencySafe: true,
		},
		remaining: remaining,
	}
}

func (t *contextRemainingTool) Execute(ctx context.Context, _ map[string]any) (*ToolResponse, error) {
	left, ok := t.remaining(ctx)
	// JSON text keeps the null case representable (codex emits null when the
	// budget is unknown); a bare number could not distinguish "unknown".
	var payload []byte
	if ok {
		payload, _ = json.Marshal(map[string]any{"tokens_left": left})
	} else {
		payload = []byte(`{"tokens_left": null}`)
	}
	return NewTextResponse(string(payload)), nil
}
