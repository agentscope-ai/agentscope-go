package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var globSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"pattern": {
			"type": "string",
			"description": "Glob pattern to match files (e.g. 'src/**/*.go', '*.json')"
		},
		"base_dir": {
			"type": "string",
			"description": "Base directory for relative patterns (default: current directory)"
		}
	},
	"required": ["pattern"]
}`)

const maxGlobResults = 1000

type globTool struct {
	BaseTool
}

func (t *globTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	raw, ok := args["pattern"]
	if !ok {
		return NewErrorResponse(fmt.Errorf("pattern is required")), nil
	}
	pattern, ok := raw.(string)
	if !ok {
		return NewErrorResponse(fmt.Errorf("pattern must be a string")), nil
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return NewErrorResponse(fmt.Errorf("pattern cannot be empty")), nil
	}

	baseDir := "."
	if v, ok := args["base_dir"].(string); ok && v != "" {
		baseDir = v
	}
	baseDir, err := filepath.Abs(baseDir)
	if err != nil {
		return NewErrorResponse(fmt.Errorf("invalid base_dir: %w", err)), nil
	}

	// Handle ** (doublestar) by walking the directory tree
	if strings.Contains(pattern, "**") {
		matches, err := globDoublestar(baseDir, pattern)
		if err != nil {
			return NewErrorResponse(fmt.Errorf("glob: %w", err)), nil
		}
		return formatGlobResult(matches), nil
	}

	// Simple glob via filepath.Glob
	fullPattern := filepath.Join(baseDir, pattern)
	matches, err := filepath.Glob(fullPattern)
	if err != nil {
		return NewErrorResponse(fmt.Errorf("glob: %w", err)), nil
	}

	return formatGlobResult(matches), nil
}

func globDoublestar(baseDir, pattern string) ([]string, error) {
	parts := strings.SplitN(pattern, "**", 2)
	prefix := parts[0]
	suffix := ""
	if len(parts) > 1 {
		suffix = parts[1]
		if strings.HasPrefix(suffix, string(filepath.Separator)) || strings.HasPrefix(suffix, "/") {
			suffix = suffix[1:]
		}
	}

	root := filepath.Join(baseDir, prefix)
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		root = filepath.Dir(root)
	}

	var matches []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if len(matches) >= maxGlobResults {
			return filepath.SkipAll
		}
		if suffix == "" {
			if !info.IsDir() {
				matches = append(matches, path)
			}
			return nil
		}
		if matched, _ := filepath.Match(suffix, info.Name()); matched {
			matches = append(matches, path)
		}
		return nil
	})
	return matches, err
}

func formatGlobResult(matches []string) *ToolResponse {
	if len(matches) == 0 {
		return NewTextResponse("No files matched.")
	}
	truncated := false
	if len(matches) > maxGlobResults {
		matches = matches[:maxGlobResults]
		truncated = true
	}
	result := strings.Join(matches, "\n")
	if truncated {
		result += fmt.Sprintf("\n... (truncated, showing first %d matches)", maxGlobResults)
	}
	return NewTextResponse(result)
}

// GlobTool returns a tool that matches files using glob patterns.
func GlobTool() Tool {
	return &globTool{
		BaseTool: BaseTool{
			ToolName:        "Glob",
			ToolDescription: "Find files matching a glob pattern. Supports ** for recursive matching. Returns matching file paths.",
			ToolSchema:      globSchema,
			ReadOnly:        true,
			ConcurrencySafe: true,
		},
	}
}
