package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

var searchMemorySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"keywords": {
			"type": "array",
			"items": {"type": "string"},
			"description": "Short, targeted search keywords to find relevant memories."
		},
		"limit": {
			"type": "integer",
			"default": 5,
			"description": "Maximum number of memories to return."
		}
	},
	"required": ["keywords"]
}`)

var addMemorySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"thinking": {
			"type": "string",
			"description": "Your reasoning for why this information should be remembered. Not stored."
		},
		"content": {
			"type": "array",
			"items": {"type": "string"},
			"description": "The information to remember. Each item is stored as a separate memory."
		}
	},
	"required": ["thinking", "content"]
}`)

// SearchMemoryTool searches the memory store for relevant memories.
type SearchMemoryTool struct {
	tool.BaseTool
	store   MemoryStore
	userID  string
	agentID string
	scoped  bool
}

func newSearchMemoryTool(store MemoryStore, userID, agentID string, scopeByAgent bool) *SearchMemoryTool {
	return &SearchMemoryTool{
		BaseTool: tool.BaseTool{
			ToolName:        "search_memory",
			ToolDescription: "Retrieve memories based on short, targeted search keywords. Use when the conversation depends on information from past interactions.",
			ToolSchema:      searchMemorySchema,
			ConcurrencySafe: true,
			ReadOnly:        true,
		},
		store:   store,
		userID:  userID,
		agentID: agentID,
		scoped:  scopeByAgent,
	}
}

func (t *SearchMemoryTool) Execute(ctx context.Context, input map[string]any) (*tool.ToolResponse, error) {
	keywordsRaw, ok := input["keywords"]
	if !ok {
		return tool.NewTextResponse("(no keywords supplied -- nothing to search)"), nil
	}
	keywordsList, ok := keywordsRaw.([]any)
	if !ok || len(keywordsList) == 0 {
		return tool.NewTextResponse("(no keywords supplied -- nothing to search)"), nil
	}

	limit := 5
	if v, ok := input["limit"]; ok {
		if f, ok := v.(float64); ok && f > 0 {
			limit = int(f)
		}
	}

	opts := &SearchOptions{TopK: limit}
	if t.scoped && t.agentID != "" {
		opts.AgentID = t.agentID
	}

	seen := make(map[string]bool)
	var allResults []Memory

	for _, kw := range keywordsList {
		kwStr, ok := kw.(string)
		if !ok || kwStr == "" {
			continue
		}
		results, err := t.store.Search(ctx, kwStr, t.userID, opts)
		if err != nil {
			continue
		}
		for _, m := range results {
			if !seen[m.ID] {
				seen[m.ID] = true
				allResults = append(allResults, m)
			}
		}
	}

	if len(allResults) == 0 {
		return tool.NewTextResponse("(no relevant memories found)"), nil
	}
	if len(allResults) > limit {
		allResults = allResults[:limit]
	}

	var sb strings.Builder
	for _, m := range allResults {
		sb.WriteString("- ")
		sb.WriteString(m.Text)
		sb.WriteString("\n")
	}
	return tool.NewTextResponse(sb.String()), nil
}

func (t *SearchMemoryTool) CheckPermissions(_ map[string]any, _ *permission.Context) permission.Decision {
	return permission.Decision{
		Behavior: permission.BehaviorAllow,
		Message:  "auto-allowed: memory search tool",
	}
}

// AddMemoryTool stores new information in the memory store.
type AddMemoryTool struct {
	tool.BaseTool
	store   MemoryStore
	userID  string
	agentID string
}

func newAddMemoryTool(store MemoryStore, userID, agentID string) *AddMemoryTool {
	return &AddMemoryTool{
		BaseTool: tool.BaseTool{
			ToolName:        "add_memory",
			ToolDescription: "Record important, durable information that may be useful in future conversations. Only store facts, preferences, or decisions -- not transient conversation details.",
			ToolSchema:      addMemorySchema,
			ConcurrencySafe: false,
			ReadOnly:        false,
		},
		store:   store,
		userID:  userID,
		agentID: agentID,
	}
}

func (t *AddMemoryTool) Execute(ctx context.Context, input map[string]any) (*tool.ToolResponse, error) {
	contentRaw, ok := input["content"]
	if !ok {
		return tool.NewErrorResponse(fmt.Errorf("content is required")), nil
	}
	contentList, ok := contentRaw.([]any)
	if !ok || len(contentList) == 0 {
		return tool.NewErrorResponse(fmt.Errorf("content must be a non-empty array")), nil
	}

	var stored int
	for _, item := range contentList {
		text, ok := item.(string)
		if !ok || text == "" {
			continue
		}
		if err := t.store.Add(ctx, text, t.userID, t.agentID); err != nil {
			return tool.NewErrorResponse(fmt.Errorf("failed to store memory: %w", err)), nil
		}
		stored++
	}

	return tool.NewTextResponse(fmt.Sprintf("Stored %d memory item(s).", stored)), nil
}

func (t *AddMemoryTool) CheckPermissions(_ map[string]any, _ *permission.Context) permission.Decision {
	return permission.Decision{
		Behavior: permission.BehaviorAllow,
		Message:  "auto-allowed: memory add tool",
	}
}

// NewMemoryTools creates the search_memory and add_memory tools for the given configuration.
func NewMemoryTools(store MemoryStore, userID, agentID string, scopeSearchByAgent bool) []tool.Tool {
	return []tool.Tool{
		newSearchMemoryTool(store, userID, agentID, scopeSearchByAgent),
		newAddMemoryTool(store, userID, agentID),
	}
}
