package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

// CompressionResult reports what an agent-driven compression actually did.
//
// Compressed=false is NOT an error: it means the context was still below the
// trigger threshold and nothing was summarized. The distinction matters because
// the tool's reply is what the model reads. Claiming "context compressed" when
// nothing happened teaches the model that details it can no longer see are
// still in context (upstream #2143 returns exactly this boolean for the same
// reason).
type CompressionResult struct {
	Compressed bool
	// Detail optionally describes the outcome (e.g. token counts before and
	// after). It is appended to the tool result when present.
	Detail string
}

// CompressFunc performs agent-driven context compression and reports whether it
// changed anything. agent.WithAgentDrivenCompression wires it to the agent's
// own compression routine.
type CompressFunc func(ctx context.Context) (CompressionResult, error)

// compressContextTool lets the model drive context compression itself
// (upstream #2143): when the history grows unwieldy the agent calls this
// tool and the wired compression function summarizes old messages, instead
// of compression only happening automatically at the token threshold.
type compressContextTool struct {
	BaseTool
	compress CompressFunc
}

var compressContextSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"reason": {
			"type": "string",
			"description": "Why compression is needed now (optional, for logs)"
		}
	}
}`)

// NewCompressContextTool creates the agent-driven compression tool. The
// compress func performs the actual compression (agent.WithAgentDrivenCompression
// wires it to UnifiedAgent.compressContextForTool). It panics on a nil func:
// programmer error, mirroring the construction contract in STABILITY.md.
//
// The tool is marked not concurrency-safe because it rewrites the agent's
// message history; running it in a parallel tool batch alongside other calls
// would let a concurrent result land in a context that is being replaced.
func NewCompressContextTool(compress CompressFunc) Tool {
	if compress == nil {
		panic("tool: NewCompressContextTool requires a non-nil compress func")
	}
	return &compressContextTool{
		BaseTool: BaseTool{
			ToolName:        "compress_context",
			ToolDescription: "Compress the conversation context now: older messages are summarized and replaced by a compact summary, freeing context space. Call this when the history grows too long for the current task, rather than waiting for automatic compression. The result tells you whether anything was actually compressed.",
			ToolSchema:      compressContextSchema,
			// Rewrites shared conversation state; must not run in a
			// parallel tool batch (upstream #2143 marks it
			// is_concurrency_safe=False).
			ConcurrencySafe: false,
		},
		compress: compress,
	}
}

func (t *compressContextTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	if reason, _ := args["reason"].(string); reason != "" {
		_ = reason // accepted for model ergonomics; the caller logs it
	}
	res, err := t.compress(ctx)
	if err != nil {
		return NewErrorResponse(fmt.Errorf("compress context: %w", err)), nil
	}
	if !res.Compressed {
		// Honest no-op. The model must not believe details were summarized
		// away when the context is unchanged.
		msg := "No compression was needed: the context is still below the compression threshold, so nothing was summarized and no detail was lost."
		if res.Detail != "" {
			msg += " " + res.Detail
		}
		return NewTextResponse(msg), nil
	}
	msg := "Context compressed: older messages were replaced by a summary. Details from those messages are no longer visible verbatim; re-read files or re-run tools if you need them exactly."
	if res.Detail != "" {
		msg += " " + res.Detail
	}
	return NewTextResponse(msg), nil
}

// CheckPermissions allows the tool without a confirmation round-trip. It is an
// internal maintenance action on the agent's own context, not an operation on
// the user's environment, so gating it behind ASK would stall every
// model-initiated compression (upstream #2143 registers it with an explicit
// allow).
func (t *compressContextTool) CheckPermissions(_ map[string]any, _ *permission.Context) permission.Decision {
	return permission.Decision{
		Behavior: permission.BehaviorAllow,
		Message:  "compress_context only rewrites the agent's own context",
	}
}
