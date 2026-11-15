package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool/lsp"
)

var lspSchema = json.RawMessage(`{
    "type": "object",
    "properties": {
        "operation": {
            "type": "string",
            "enum": ["goToDefinition", "findReferences", "hover", "documentSymbol", "workspaceSymbol"],
            "description": "The LSP operation to perform"
        },
        "filePath": {
            "type": "string",
            "description": "The absolute path to the file to operate on"
        },
        "line": {
            "type": "integer",
            "description": "The line number (1-based)"
        },
        "character": {
            "type": "integer",
            "description": "The character offset (1-based)"
        },
        "query": {
            "type": "string",
            "description": "The symbol name to search for (workspaceSymbol only)"
        }
    },
    "required": ["operation", "filePath"]
}`)

type lspTool struct {
	BaseTool
	mu      sync.Mutex
	servers map[string]*lsp.Server
	rootDir string
	configs map[string]lsp.ServerConfig
}

type LSPOption func(*lspTool)

func WithLSPRootDir(dir string) LSPOption {
	return func(t *lspTool) { t.rootDir = dir }
}

func WithLSPServerConfig(lang string, cfg lsp.ServerConfig) LSPOption {
	return func(t *lspTool) { t.configs[lang] = cfg }
}

var defaultLSPConfigs = map[string]lsp.ServerConfig{
	"go":         {Command: "gopls", Args: []string{"serve"}},
	"typescript": {Command: "typescript-language-server", Args: []string{"--stdio"}},
	"python":     {Command: "pylsp"},
}

func (t *lspTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	operation, _ := args["operation"].(string)
	filePath, _ := args["filePath"].(string)
	if operation == "" {
		return NewErrorResponse(fmt.Errorf("operation is required")), nil
	}
	if filePath == "" {
		return NewErrorResponse(fmt.Errorf("filePath is required")), nil
	}

	line := 0
	if v, ok := args["line"].(float64); ok {
		line = int(v) - 1
		if line < 0 {
			line = 0
		}
	}
	char := 0
	if v, ok := args["character"].(float64); ok {
		char = int(v) - 1
		if char < 0 {
			char = 0
		}
	}
	query, _ := args["query"].(string)

	srv, err := t.getServer(ctx, filePath)
	if err != nil {
		return NewErrorResponse(err), nil
	}

	switch operation {
	case "goToDefinition":
		locs, err := srv.Definition(ctx, filePath, line, char)
		if err != nil {
			return NewErrorResponse(fmt.Errorf("definition: %w", err)), nil
		}
		return NewTextResponse(lsp.FormatLocations(locs)), nil

	case "findReferences":
		locs, err := srv.References(ctx, filePath, line, char)
		if err != nil {
			return NewErrorResponse(fmt.Errorf("references: %w", err)), nil
		}
		return NewTextResponse(lsp.FormatLocations(locs)), nil

	case "hover":
		hover, err := srv.Hover(ctx, filePath, line, char)
		if err != nil {
			return NewErrorResponse(fmt.Errorf("hover: %w", err)), nil
		}
		return NewTextResponse(lsp.FormatHover(hover)), nil

	case "documentSymbol":
		symbols, err := srv.DocumentSymbols(ctx, filePath)
		if err != nil {
			return NewErrorResponse(fmt.Errorf("document symbols: %w", err)), nil
		}
		return NewTextResponse(lsp.FormatDocumentSymbols(symbols)), nil

	case "workspaceSymbol":
		if query == "" {
			return NewErrorResponse(fmt.Errorf("query is required for workspaceSymbol")), nil
		}
		symbols, err := srv.WorkspaceSymbol(ctx, query)
		if err != nil {
			return NewErrorResponse(fmt.Errorf("workspace symbol: %w", err)), nil
		}
		return NewTextResponse(lsp.FormatWorkspaceSymbols(symbols)), nil

	default:
		return NewErrorResponse(fmt.Errorf("unknown operation: %s", operation)), nil
	}
}

func (t *lspTool) getServer(ctx context.Context, filePath string) (*lsp.Server, error) {
	lang := detectLang(filePath)
	if lang == "" {
		return nil, fmt.Errorf("unsupported file type: %s", filepath.Ext(filePath))
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if srv, ok := t.servers[lang]; ok {
		return srv, nil
	}

	cfg, ok := t.configs[lang]
	if !ok {
		cfg, ok = defaultLSPConfigs[lang]
	}
	if !ok {
		return nil, fmt.Errorf("no LSP server configured for language: %s", lang)
	}

	if cfg.RootDir == "" {
		cfg.RootDir = t.rootDir
	}

	srv := lsp.NewServer(cfg)
	if err := srv.Start(ctx); err != nil {
		return nil, fmt.Errorf("start LSP server (%s): %w", cfg.Command, err)
	}

	t.servers[lang] = srv
	return srv, nil
}

func (t *lspTool) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, srv := range t.servers {
		_ = srv.Shutdown(context.Background())
	}
	t.servers = make(map[string]*lsp.Server)
	return nil
}

func detectLang(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return "go"
	case ".ts", ".tsx", ".js", ".jsx":
		return "typescript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".c", ".h", ".cpp", ".cc", ".cxx", ".hpp":
		return "cpp"
	default:
		return ""
	}
}

func LSPTool(opts ...LSPOption) Tool {
	t := &lspTool{
		BaseTool: BaseTool{
			ToolName:        "LSP",
			ToolDescription: "Interact with Language Server Protocol servers to get code intelligence (go-to-definition, find-references, hover, symbols).",
			ToolSchema:      lspSchema,
			ReadOnly:        true,
			ConcurrencySafe: true,
		},
		servers: make(map[string]*lsp.Server),
		configs: make(map[string]lsp.ServerConfig),
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

var _ Tool = (*lspTool)(nil)
