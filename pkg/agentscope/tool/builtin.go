package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/platform"
)

const (
	MaxFileSize         = 1024 * 1024
	DefaultShellTimeout = 30

	// MaxInlineImageBytes caps images that Read returns as base64 DataBlocks.
	// The token estimator decodes base64 back to raw bytes (len*3/4) and then
	// divides the total by 4, so an inlined image costs roughly fileSize/4
	// tokens: 256KB is about 64k tokens, already a large fraction of a context
	// window. Bigger images are reported as text (name, media type, size)
	// instead of being dropped without notice or blowing the budget.
	MaxInlineImageBytes = 256 * 1024
)

// --- execute_shell_command ---

var shellCommandSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"command": {"type": "string", "description": "The shell command to execute"},
		"timeout": {"type": "number", "description": "Timeout in seconds (default 30, max 300)"}
	},
	"required": ["command"]
}`)

type shellCommandTool struct {
	BaseTool
}

func (t *shellCommandTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	raw, ok := args["command"]
	if !ok {
		return NewErrorResponse(fmt.Errorf("command is required")), nil
	}
	cmdStr, ok := raw.(string)
	if !ok {
		return NewErrorResponse(fmt.Errorf("command must be a string")), nil
	}
	cmdStr = strings.TrimSpace(cmdStr)
	if cmdStr == "" {
		return NewErrorResponse(fmt.Errorf("command cannot be empty")), nil
	}

	timeoutSec := DefaultShellTimeout
	if t, ok := args["timeout"]; ok {
		switch v := t.(type) {
		case float64:
			timeoutSec = int(v)
		case int:
			timeoutSec = v
		}
		if timeoutSec <= 0 {
			timeoutSec = DefaultShellTimeout
		}
		if timeoutSec > 300 {
			timeoutSec = 300
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := platform.Command(runCtx, cmdStr)
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	result := map[string]any{
		"exit_code": exitCode,
		"output":    string(out),
	}
	if err != nil {
		result["error"] = err.Error()
	}

	b, _ := json.Marshal(result)
	return NewTextResponse(string(b)), nil
}

// ExecuteShellCommandTool returns a tool that runs shell commands.
func ExecuteShellCommandTool() Tool {
	return &shellCommandTool{
		BaseTool: BaseTool{
			ToolName:        "execute_shell_command",
			ToolDescription: "Execute a shell command and return output. Args: command (string, required), timeout (number, optional, seconds, default 30).",
			ToolSchema:      shellCommandSchema,
		},
	}
}

// --- view_text_file ---

var viewTextFileSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {"type": "string", "description": "Path to the text file to read"},
		"file_path": {"type": "string", "description": "Alternative key for file path"}
	}
}`)

type viewTextFileTool struct {
	BaseTool
}

func (t *viewTextFileTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	path := ""
	for _, k := range []string{"path", "file_path"} {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok {
				path = strings.TrimSpace(s)
				break
			}
		}
	}
	if path == "" {
		return NewErrorResponse(fmt.Errorf("path or file_path is required")), nil
	}

	clean := filepath.Clean(path)
	abs, err := filepath.Abs(clean)
	if err != nil {
		return NewErrorResponse(fmt.Errorf("invalid path: %w", err)), nil
	}

	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return NewErrorResponse(fmt.Errorf("file not found: %s", path)), nil
		}
		return NewErrorResponse(fmt.Errorf("stat: %w", err)), nil
	}
	if info.IsDir() {
		return NewErrorResponse(fmt.Errorf("path is a directory, not a file: %s", path)), nil
	}
	if info.Size() > MaxFileSize {
		return NewErrorResponse(fmt.Errorf("file too large (max %d bytes): %s", MaxFileSize, path)), nil
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return NewErrorResponse(fmt.Errorf("read file: %w", err)), nil
	}

	result := map[string]any{
		"path":    abs,
		"content": string(data),
	}
	b, _ := json.Marshal(result)
	return NewTextResponse(string(b)), nil
}

// ViewTextFileTool returns a tool that reads text files.
func ViewTextFileTool() Tool {
	return &viewTextFileTool{
		BaseTool: BaseTool{
			ToolName:        "view_text_file",
			ToolDescription: "Read the contents of a text file (max 1MB). Args: path or file_path (string, required).",
			ToolSchema:      viewTextFileSchema,
			ReadOnly:        true,
			ConcurrencySafe: true,
		},
	}
}

// NewEnhancedToolkit returns a Toolkit with the full v2 tool set:
// bash, read, write, edit, glob, grep.
func NewEnhancedToolkit() *Toolkit {
	return NewToolkit(
		BashTool(),
		ReadTool(),
		WriteTool(),
		EditTool(),
		MultiEditTool(),
		ApplyPatchTool(),
		GlobTool(),
		GrepTool(),
	)
}
