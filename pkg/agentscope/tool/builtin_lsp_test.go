package tool

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestLSPTool_Creation(t *testing.T) {
	tool := LSPTool()
	if tool.Name() != "LSP" {
		t.Fatalf("expected name LSP, got %s", tool.Name())
	}
	if !tool.IsReadOnly() {
		t.Fatal("LSP tool should be read-only")
	}
}

func TestLSPTool_MissingOperation(t *testing.T) {
	tool := LSPTool()
	resp, err := tool.Execute(context.Background(), map[string]any{
		"filePath": "/test/main.go",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("expected error state for missing operation, got %s", resp.State)
	}
}

func TestLSPTool_MissingFilePath(t *testing.T) {
	tool := LSPTool()
	resp, err := tool.Execute(context.Background(), map[string]any{
		"operation": "hover",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("expected error state for missing filePath, got %s", resp.State)
	}
}

func TestLSPTool_UnsupportedExtension(t *testing.T) {
	tool := LSPTool()
	resp, err := tool.Execute(context.Background(), map[string]any{
		"operation": "hover",
		"filePath":  "/test/file.xyz",
		"line":      float64(1),
		"character": float64(1),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("expected error state for unsupported extension, got %s", resp.State)
	}
}

func TestDetectLang(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/main.go", "go"},
		{"/app.ts", "typescript"},
		{"/app.tsx", "typescript"},
		{"/app.js", "typescript"},
		{"/app.jsx", "typescript"},
		{"/script.py", "python"},
		{"/main.rs", "rust"},
		{"/Main.java", "java"},
		{"/main.c", "cpp"},
		{"/main.cpp", "cpp"},
		{"/file.txt", ""},
		{"/file.xyz", ""},
	}
	for _, tt := range tests {
		got := detectLang(tt.path)
		if got != tt.want {
			t.Errorf("detectLang(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}
