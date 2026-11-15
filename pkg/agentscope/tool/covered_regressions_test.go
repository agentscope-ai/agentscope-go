package tool

// Regression locks for behaviors already covered in Go, verified against the
// upstream Python v2.0.7 window (PORTING_PLAN.md section 10.1):
//   - #2316: inactive tool groups are distinguished from missing tools
//   - #2378: FunctionTool custom input schemas are honored end-to-end

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

func TestCallTool_InactiveGroupDistinguishedFromMissing(t *testing.T) {
	// Upstream #2316: calling a tool whose group is inactive must say so
	// (with an activation hint), not report the tool as missing.
	tk := NewToolkit()
	tk.AddGroup("search", NewFunctionTool("web_search", "search", nil,
		func(_ context.Context, _ map[string]any) (any, error) { return "ok", nil }))
	tk.DeactivateGroup("search")

	_, err := tk.CallTool(context.Background(), "web_search", map[string]any{})
	if err == nil {
		t.Fatal("expected error for tool in inactive group")
	}
	var inactiveErr *agenterrors.ToolGroupInactiveError
	if !errors.As(err, &inactiveErr) {
		t.Fatalf("expected ToolGroupInactiveError, got %T: %v", err, err)
	}
	if inactiveErr.GroupName != "search" {
		t.Errorf("GroupName = %q, want search", inactiveErr.GroupName)
	}
	if !strings.Contains(inactiveErr.AgentMessage(), "Activate the group") {
		t.Errorf("agent-facing message should hint activation, got %q", inactiveErr.AgentMessage())
	}

	// A genuinely unknown tool must surface the distinct not-found error.
	_, err = tk.CallTool(context.Background(), "no_such_tool", map[string]any{})
	var notFoundErr *agenterrors.ToolNotFoundError
	if !errors.As(err, &notFoundErr) {
		t.Fatalf("expected ToolNotFoundError for unknown tool, got %T: %v", err, err)
	}
}

func TestFunctionTool_CustomSchemaHonored(t *testing.T) {
	// Upstream #2378 class: callers supply their own input schema; it must
	// reach model-facing tool schemas unchanged (Go takes it one step
	// further: the schema is always explicit, never auto-generated).
	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)
	ft := NewFunctionTool("search", "custom schema tool", schema,
		func(_ context.Context, _ map[string]any) (any, error) { return "ok", nil })

	if string(ft.InputSchema()) != string(schema) {
		t.Fatalf("InputSchema() = %s, want the custom schema", ft.InputSchema())
	}

	tk := NewToolkit(ft)
	schemas := tk.GetToolSchemas()
	if len(schemas) != 1 {
		t.Fatalf("expected 1 schema, got %d", len(schemas))
	}
	if schemas[0].Function.Name != "search" {
		t.Errorf("schema name = %q", schemas[0].Function.Name)
	}
	if string(schemas[0].Function.Parameters) != string(schema) {
		t.Errorf("tool schema parameters = %s, want the custom schema verbatim", schemas[0].Function.Parameters)
	}
}

func TestToolkitRejectsDuplicateToolNames(t *testing.T) {
	tk := NewToolkit(NewFunctionTool("search", "first", nil,
		func(_ context.Context, _ map[string]any) (any, error) { return "first", nil }))

	defer func() {
		if recover() == nil {
			t.Fatal("expected AddGroup to reject a case-insensitive duplicate tool name")
		}
	}()
	tk.AddGroup("other", NewFunctionTool("SEARCH", "second", nil,
		func(_ context.Context, _ map[string]any) (any, error) { return "second", nil }))
}

func TestToolkitSchemasHaveDeterministicGroupOrder(t *testing.T) {
	tk := NewToolkit()
	tk.AddGroup("z-last", NewFunctionTool("z", "z", nil,
		func(_ context.Context, _ map[string]any) (any, error) { return "z", nil }))
	tk.AddGroup("a-first", NewFunctionTool("a", "a", nil,
		func(_ context.Context, _ map[string]any) (any, error) { return "a", nil }))

	schemas := tk.GetToolSchemas()
	if len(schemas) != 2 {
		t.Fatalf("expected 2 schemas, got %d", len(schemas))
	}
	if schemas[0].Function.Name != "a" || schemas[1].Function.Name != "z" {
		t.Fatalf("schema order = [%s, %s], want [a, z]", schemas[0].Function.Name, schemas[1].Function.Name)
	}
}
