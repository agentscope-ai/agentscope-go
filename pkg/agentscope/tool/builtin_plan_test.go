package tool

import (
	"context"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestValidatePlanUpdate(t *testing.T) {
	cases := []struct {
		name    string
		plan    []PlanItem
		wantErr string
	}{
		{"empty clears", nil, ""},
		{"single in_progress", []PlanItem{{Step: "a", Status: "in_progress"}}, ""},
		{"two in_progress", []PlanItem{{Step: "a", Status: "in_progress"}, {Step: "b", Status: "in_progress"}}, "at most one"},
		{"empty step", []PlanItem{{Step: "", Status: "pending"}}, "empty"},
		{"unknown status", []PlanItem{{Step: "a", Status: "done"}}, "unknown status"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidatePlanUpdate(c.plan)
			if c.wantErr == "" && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Errorf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestUpdatePlanTool_Execute(t *testing.T) {
	var got []PlanItem
	var gotExpl string
	tool2 := NewUpdatePlanTool(func(_ context.Context, expl string, plan []PlanItem) error {
		gotExpl = expl
		got = plan
		return nil
	})

	resp, err := tool2.Execute(context.Background(), map[string]any{
		"explanation": "phase 2",
		"plan": []any{
			map[string]any{"step": "design", "status": "completed"},
			map[string]any{"step": "build", "status": "in_progress"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %v", resp.State)
	}
	if len(got) != 2 || got[1].Step != "build" || gotExpl != "phase 2" {
		t.Errorf("callback got wrong data: %+v expl=%q", got, gotExpl)
	}
	// The metadata marker carries the plan for the agent's live-render event.
	md, ok := resp.Metadata[PlanMetadataKey].(PlanMetadata)
	if !ok || len(md.Plan) != 2 || md.Explanation != "phase 2" {
		t.Errorf("metadata marker wrong: %+v", resp.Metadata)
	}

	// Missing plan arg.
	resp, _ = tool2.Execute(context.Background(), map[string]any{})
	if resp.State != message.ToolResultError {
		t.Error("missing plan must be a tool error")
	}
	// Malformed items.
	resp, _ = tool2.Execute(context.Background(), map[string]any{"plan": "not-a-list"})
	if resp.State != message.ToolResultError {
		t.Error("malformed plan must be a tool error")
	}
	// Validation failure does not reach the callback.
	got = nil
	resp, _ = tool2.Execute(context.Background(), map[string]any{
		"plan": []any{
			map[string]any{"step": "a", "status": "in_progress"},
			map[string]any{"step": "b", "status": "in_progress"},
		},
	})
	if resp.State != message.ToolResultError || got != nil {
		t.Errorf("invalid plan must be rejected before the callback: state=%v got=%v", resp.State, got)
	}
}
