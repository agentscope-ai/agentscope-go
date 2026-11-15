package agent

import (
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

func TestBuildStateContext_NilState(t *testing.T) {
	result := BuildStateContext(nil)
	if result != "" {
		t.Errorf("expected empty string for nil state, got %q", result)
	}
}

func TestBuildStateContext_PopulatedState(t *testing.T) {
	state := &AgentState{
		SessionID: "sess-abc-123",
		CurIter:   5,
		PermissionCtx: &permission.Context{
			Mode: permission.ModeBypass,
		},
		ToolCtx: &ToolStateContext{
			ActivatedGroups: []string{"file_ops", "network"},
		},
		TasksCtx: &TasksStateContext{
			Tasks: []TaskState{
				{ID: "t1", Subject: "Fix bug", State: "in_progress", Owner: "alice"},
				{ID: "t2", Subject: "Write tests", State: "pending"},
			},
		},
	}

	result := BuildStateContext(state)

	checks := []struct {
		label    string
		contains string
	}{
		{"session ID", "sess-abc-123"},
		{"iteration", "Iteration: 5"},
		{"permission mode", "bypass"},
		{"tool group file_ops", "file_ops"},
		{"tool group network", "network"},
		{"task 1 subject", "Fix bug"},
		{"task 1 state", "in_progress"},
		{"task 1 owner", "alice"},
		{"task 2 subject", "Write tests"},
		{"task 2 state", "pending"},
	}
	for _, c := range checks {
		if !strings.Contains(result, c.contains) {
			t.Errorf("BuildStateContext missing %s (%q) in:\n%s", c.label, c.contains, result)
		}
	}
}

func TestBuildStateContext_PartialState(t *testing.T) {
	state := &AgentState{
		CurIter: 0,
	}
	result := BuildStateContext(state)
	if !strings.Contains(result, "Iteration: 0") {
		t.Errorf("expected iteration in output, got %q", result)
	}
	// No permission, tool, or tasks sections
	if strings.Contains(result, "Permission Mode") {
		t.Error("should not contain Permission Mode when PermissionCtx is nil")
	}
	if strings.Contains(result, "Active Tool Groups") {
		t.Error("should not contain Active Tool Groups when ToolCtx is nil")
	}
	if strings.Contains(result, "Active Tasks") {
		t.Error("should not contain Active Tasks when TasksCtx is nil")
	}
}

func TestInjectStateAwareness_NilState(t *testing.T) {
	original := "You are a helpful assistant."
	result := InjectStateAwareness(original, nil)
	if result != original {
		t.Errorf("expected original prompt unchanged, got %q", result)
	}
}

func TestInjectStateAwareness_WithState(t *testing.T) {
	original := "You are a helpful assistant."
	state := &AgentState{
		SessionID: "sess-xyz",
		CurIter:   3,
		PermissionCtx: &permission.Context{
			Mode: permission.ModeDefault,
		},
	}

	result := InjectStateAwareness(original, state)

	// Must start with original prompt
	if !strings.HasPrefix(result, original) {
		t.Errorf("result should start with original prompt, got %q", result)
	}

	// Must contain agent_state tags
	if !strings.Contains(result, "<agent_state>") {
		t.Error("result should contain <agent_state> opening tag")
	}
	if !strings.Contains(result, "</agent_state>") {
		t.Error("result should contain </agent_state> closing tag")
	}

	// Must contain state data inside the block
	if !strings.Contains(result, "sess-xyz") {
		t.Error("result should contain session ID")
	}
	if !strings.Contains(result, "Iteration: 3") {
		t.Error("result should contain iteration")
	}
}

func TestInjectStateAwareness_EmptyToolCtx(t *testing.T) {
	original := "Base prompt."
	state := &AgentState{
		CurIter: 1,
		ToolCtx: &ToolStateContext{
			ActivatedGroups: []string{}, // empty slice
		},
	}

	result := InjectStateAwareness(original, state)

	// Even with empty tool groups, the state block should still be injected
	// because CurIter produces output.
	if !strings.Contains(result, "<agent_state>") {
		t.Error("expected agent_state block for non-nil state with iteration")
	}
	// But no "Active Tool Groups" line since the slice is empty.
	if strings.Contains(result, "Active Tool Groups") {
		t.Error("should not mention Active Tool Groups for empty slice")
	}
}
