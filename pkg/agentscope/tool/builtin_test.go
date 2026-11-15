package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestExecuteShellCommandTool(t *testing.T) {
	tool := ExecuteShellCommandTool()
	if tool.Name() != "execute_shell_command" {
		t.Fatalf("unexpected name: %s", tool.Name())
	}

	ctx := context.Background()

	// Success case
	resp, err := tool.Execute(ctx, map[string]any{"command": "echo hello"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("expected success state, got %s", resp.State)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "hello") {
		t.Fatalf("unexpected output: %q", text)
	}

	// Missing command
	resp, err = tool.Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatal("expected error state for missing command")
	}
}

func TestViewTextFileTool(t *testing.T) {
	tool := ViewTextFileTool()
	if tool.Name() != "view_text_file" {
		t.Fatalf("unexpected name: %s", tool.Name())
	}

	ctx := context.Background()

	// Create temp file
	tmp, err := os.CreateTemp("", "agentscope-view-*.txt")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(tmp.Name())
	_, _ = tmp.WriteString("hello world\n")
	tmp.Close()

	// Success case with "path"
	resp, err := tool.Execute(ctx, map[string]any{"path": tmp.Name()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("expected success, got %s", resp.State)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "hello world") {
		t.Fatalf("unexpected content: %q", text)
	}

	// Success case with "file_path"
	resp, err = tool.Execute(ctx, map[string]any{"file_path": tmp.Name()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("expected success, got %s", resp.State)
	}

	// File not found
	resp, err = tool.Execute(ctx, map[string]any{"path": filepath.Join(os.TempDir(), "nonexistent-12345.txt")})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatal("expected error state for nonexistent file")
	}

	// Missing path
	resp, err = tool.Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatal("expected error state for missing path")
	}
}

func TestFunctionTool(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"x":{"type":"number"},"y":{"type":"number"}},"required":["x","y"]}`)
	ft := NewFunctionTool("add", "Add two numbers", schema, func(ctx context.Context, input map[string]any) (any, error) {
		x := input["x"].(float64)
		y := input["y"].(float64)
		return x + y, nil
	})

	if ft.Name() != "add" {
		t.Fatalf("unexpected name: %s", ft.Name())
	}

	resp, err := ft.Execute(context.Background(), map[string]any{"x": 3.0, "y": 4.0})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("expected success, got %s", resp.State)
	}
	text := getResponseText(resp)
	if text != "7" {
		t.Fatalf("expected '7', got %q", text)
	}
}

func TestToolkitGroups(t *testing.T) {
	tk := NewToolkit()
	schema := json.RawMessage(`{"type":"object","properties":{}}`)

	t1 := NewFunctionTool("tool1", "desc1", schema, func(ctx context.Context, input map[string]any) (any, error) { return "ok", nil })
	t2 := NewFunctionTool("tool2", "desc2", schema, func(ctx context.Context, input map[string]any) (any, error) { return "ok", nil })

	tk.AddGroup("extra", t1, t2)

	if tk.Get("tool1") == nil {
		t.Fatal("tool1 should be findable")
	}

	schemas := tk.GetToolSchemas()
	if len(schemas) != 2 {
		t.Fatalf("expected 2 schemas, got %d", len(schemas))
	}

	tk.DeactivateGroup("extra")
	if tk.Get("tool1") != nil {
		t.Fatal("tool1 should not be findable after deactivation")
	}
	schemas = tk.GetToolSchemas()
	if len(schemas) != 0 {
		t.Fatalf("expected 0 schemas after deactivation, got %d", len(schemas))
	}

	tk.ActivateGroup("extra")
	if tk.Get("tool1") == nil {
		t.Fatal("tool1 should be findable after reactivation")
	}
}

func getResponseText(resp *ToolResponse) string {
	for _, b := range resp.Content {
		if tb, ok := b.(message.TextBlock); ok {
			return tb.Text
		}
	}
	return ""
}
