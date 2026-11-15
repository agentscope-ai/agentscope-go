package tool

import (
	"context"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func resetToolsText(t *testing.T, resp *ToolResponse) string {
	t.Helper()
	for _, b := range resp.Content {
		if tb, ok := b.(message.TextBlock); ok {
			return tb.Text
		}
	}
	return ""
}

func TestResetToolsRejectsUnknownGroups(t *testing.T) {
	tk := NewToolkit()
	tk.AddGroup("search")
	tk.AddGroup("coding")
	tool := ResetToolsTool(tk)

	// One valid, one unknown group: must reject and change nothing.
	resp, err := tool.Execute(context.Background(), map[string]any{
		"activate":   []any{"search", "nonexistent"},
		"deactivate": []any{"coding"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("state = %v, want %v", resp.State, message.ToolResultError)
	}
	text := resetToolsText(t, resp)
	if !strings.Contains(text, "nonexistent") {
		t.Errorf("response should name the invalid group, got %q", text)
	}
	if !strings.Contains(text, "search") || !strings.Contains(text, "coding") {
		t.Errorf("response should list available groups, got %q", text)
	}
	// No state change may have happened: coding must still be active.
	if !tk.IsGroupActive("coding") {
		t.Error("coding group was deactivated despite validation failure")
	}
	if !tk.IsGroupActive("search") {
		t.Error("search group state changed despite validation failure")
	}
}

func TestResetToolsRejectsBasicGroup(t *testing.T) {
	dummy := NewFunctionTool("dummy", "dummy tool", nil,
		func(_ context.Context, _ map[string]any) (any, error) { return "ok", nil })
	tk := NewToolkit(dummy) // creates the always-active "basic" group
	tk.AddGroup("search")
	tool := ResetToolsTool(tk)

	resp, err := tool.Execute(context.Background(), map[string]any{
		"deactivate": []any{"basic"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("state = %v, want error (basic group must not be toggleable)", resp.State)
	}
	if !tk.IsGroupActive("basic") {
		t.Error("basic group was deactivated")
	}
}

func TestResetToolsRejectsNonArrayArg(t *testing.T) {
	tk := NewToolkit()
	tk.AddGroup("search")
	tool := ResetToolsTool(tk)

	resp, err := tool.Execute(context.Background(), map[string]any{
		"activate": "search", // not an array
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("state = %v, want error for non-array argument", resp.State)
	}
}

func TestResetToolsAppliesValidChanges(t *testing.T) {
	tk := NewToolkit()
	tk.AddGroup("search")
	tk.AddGroup("coding")
	tool := ResetToolsTool(tk)

	resp, err := tool.Execute(context.Background(), map[string]any{
		"activate":   []any{"coding"},
		"deactivate": []any{"search"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.State == message.ToolResultError {
		t.Fatalf("unexpected error response: %q", resetToolsText(t, resp))
	}
	if !tk.IsGroupActive("coding") {
		t.Error("coding group not activated")
	}
	if tk.IsGroupActive("search") {
		t.Error("search group not deactivated")
	}
}

func TestResetToolsNoChanges(t *testing.T) {
	tk := NewToolkit()
	tk.AddGroup("search")
	tool := ResetToolsTool(tk)

	resp, err := tool.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.State == message.ToolResultError {
		t.Fatalf("unexpected error response: %q", resetToolsText(t, resp))
	}
	if !strings.Contains(resetToolsText(t, resp), "No changes") {
		t.Errorf("expected no-changes message, got %q", resetToolsText(t, resp))
	}
}

func TestResetToolsRejectsNonStringElements(t *testing.T) {
	// Evaluator M7: non-string array elements must be rejected, not silently
	// dropped. `activate: [1]` is an invalid argument, not a no-op.
	tk := NewToolkit()
	tk.AddGroup("search")
	tk.DeactivateGroup("search") // AddGroup auto-activates; start inactive
	tool := ResetToolsTool(tk)

	for _, arg := range []any{
		[]any{1},
		[]any{"search", 42},
		[]any{nil},
	} {
		resp, err := tool.Execute(context.Background(), map[string]any{"activate": arg})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if resp.State != message.ToolResultError {
			t.Errorf("activate=%v: state = %v, want error for non-string element", arg, resp.State)
		}
	}
	// Nothing may have been activated.
	if tk.IsGroupActive("search") {
		t.Error("search group was activated despite invalid elements")
	}
}

func TestResetToolsEmptyAvailableGroupsMessage(t *testing.T) {
	// When only "basic" exists, the error must not end with an empty
	// "available groups are: " list.
	dummy := NewFunctionTool("dummy", "dummy tool", nil,
		func(_ context.Context, _ map[string]any) (any, error) { return "ok", nil })
	tk := NewToolkit(dummy)
	tool := ResetToolsTool(tk)

	resp, err := tool.Execute(context.Background(), map[string]any{
		"activate": []any{"ghost"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	text := resetToolsText(t, resp)
	if strings.Contains(text, "groups are: \n") || strings.HasSuffix(text, "groups are: ") {
		t.Errorf("error message has an empty available-groups list: %q", text)
	}
	if !strings.Contains(text, "(none)") {
		t.Errorf("expected (none) placeholder for empty group list, got %q", text)
	}
}
