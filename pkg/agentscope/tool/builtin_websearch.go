package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SearchResult is one web search hit.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// SearchFunc runs a web search and returns ranked hits. The host provides
// the implementation (provider API keys and ranking live outside this
// library); it must be safe for concurrent use.
type SearchFunc func(ctx context.Context, query string, limit int) ([]SearchResult, error)

var webSearchSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"query": {
			"type": "string",
			"description": "The search query"
		},
		"limit": {
			"type": "integer",
			"description": "Maximum number of results (default 5, max 10)"
		}
	},
	"required": ["query"]
}`)

const (
	defaultSearchLimit = 5
	maxSearchLimit     = 10
)

type webSearchTool struct {
	BaseTool
	search SearchFunc
}

// NewWebSearchTool creates the web_search tool (codex parity): the model
// searches the web through a host-injected SearchFunc, keeping provider
// credentials and ranking outside the library. Panics on a nil func,
// mirroring NewCompressContextTool. Read-only and concurrency-safe on the
// tool side; the SearchFunc must be concurrency-safe too.
func NewWebSearchTool(search SearchFunc) Tool {
	if search == nil {
		panic("tool: NewWebSearchTool requires a non-nil search func")
	}
	return &webSearchTool{
		BaseTool: BaseTool{
			ToolName:        "web_search",
			ToolDescription: "Search the web for current information. Returns ranked results with title, URL and snippet. Follow up with WebFetch to read a page in full.",
			ToolSchema:      webSearchSchema,
			ReadOnly:        true,
			ConcurrencySafe: true,
		},
		search: search,
	}
}

func (t *webSearchTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	query, _ := args["query"].(string)
	if strings.TrimSpace(query) == "" {
		return NewErrorResponse(fmt.Errorf("query is required")), nil
	}
	limit := defaultSearchLimit
	if v, ok := args["limit"].(float64); ok && v > 0 {
		// Clamp in the float domain first so even a huge literal (e.g.
		// 1e300) converts deterministically; then keep an integer-typed
		// bounds check on the narrowing conversion — that is the guard form
		// CodeQL's go/incorrect-integer-conversion recognizes (float
		// comparisons do not count).
		if v > maxSearchLimit {
			v = maxSearchLimit
		}
		if n := int64(v); n > maxSearchLimit {
			limit = maxSearchLimit
		} else {
			limit = int(n)
		}
	}
	results, err := t.search(ctx, query, limit)
	if err != nil {
		return NewErrorResponse(fmt.Errorf("web search: %w", err)), nil
	}
	payload, err := json.Marshal(map[string]any{"results": results})
	if err != nil {
		return NewErrorResponse(fmt.Errorf("web search: encode results: %w", err)), nil
	}
	return NewTextResponse(string(payload)), nil
}
