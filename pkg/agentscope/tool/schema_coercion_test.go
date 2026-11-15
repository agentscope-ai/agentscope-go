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

// jsonStringArg returns s as a JSON string literal. Use it for any runtime
// value embedded in a JSON literal, not only paths: these tests embed a real
// temp-dir path, and on Windows that path contains backslashes that would form
// invalid escapes (a backslash before U, for one) if concatenated raw.
func jsonStringArg(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Upstream #2496: schema-guided coercion applies to EVERY call — models
// routinely quote numbers in otherwise-valid JSON, and validation must see
// the coerced value.
func TestCallToolFromBlockCoercesQuotedInteger(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "three.txt")
	if err := os.WriteFile(f, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk := NewToolkit(&readTool{BaseTool{ToolName: "Read", ToolDescription: "read", ToolSchema: readSchema}})

	// Valid JSON, but limit is a quoted string — schema says integer.
	block := &message.ToolCallBlock{
		Type: "tool_call", ID: "c1", Name: "Read",
		Input: `{"file_path":` + jsonStringArg(t, f) + `,"limit":"1"}`,
	}
	resp, err := tk.CallToolFromBlock(context.Background(), block)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.State == message.ToolResultError {
		t.Fatalf("coercion should make the call pass validation, got error: %v", resp.Content)
	}
	text := resp.Content[0].(message.TextBlock).Text
	if strings.Count(text, "\n") > 1 || strings.Contains(text, "b") {
		t.Errorf("limit=1 not honored after coercion: %q", text)
	}
}

// Broken JSON with a quoted integer still works: syntax repair + coercion.
func TestCallToolFromBlockRepairsAndCoerces(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(f, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk := NewToolkit(&readTool{BaseTool{ToolName: "Read", ToolDescription: "read", ToolSchema: readSchema}})
	block := &message.ToolCallBlock{
		Type: "tool_call", ID: "c1", Name: "Read",
		Input: `{"file_path":` + jsonStringArg(t, f) + `,"limit":"1",}`, // trailing comma
	}
	resp, err := tk.CallToolFromBlock(context.Background(), block)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.State == message.ToolResultError {
		t.Fatalf("repair+coercion failed: %v", resp.Content)
	}
}
