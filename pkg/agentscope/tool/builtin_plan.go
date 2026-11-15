package tool

import (
	"context"
	"encoding/json"
	"fmt"
)

// PlanItem is one step of an agent-maintained task plan.
type PlanItem struct {
	Step   string `json:"step"`
	Status string `json:"status"` // PlanStatusPending / PlanStatusInProgress / PlanStatusCompleted
}

// Plan status values, mirroring the codex update_plan contract.
const (
	PlanStatusPending    = "pending"
	PlanStatusInProgress = "in_progress"
	PlanStatusCompleted  = "completed"
)

// PlanMetadataKey marks a tool response carrying a plan update. The agent
// layer turns it into a plan_updated custom event so hosts can render the
// plan live; the value is a PlanMetadata payload.
const PlanMetadataKey = "plan_update"

// PlanMetadata is the metadata payload under PlanMetadataKey.
type PlanMetadata struct {
	Explanation string     `json:"explanation,omitempty"`
	Plan        []PlanItem `json:"plan"`
}

// maxPlanItems bounds the plan size; a runaway plan would bloat every model
// call once the agent injects it into the prompt.
const maxPlanItems = 64

// ValidatePlanUpdate enforces the plan contract: at most one in_progress step
// (the model must serialize its focus), non-empty steps, bounded length.
// An empty plan is valid and clears the current plan.
func ValidatePlanUpdate(plan []PlanItem) error {
	if len(plan) > maxPlanItems {
		return fmt.Errorf("plan has %d steps, max %d", len(plan), maxPlanItems)
	}
	inProgress := 0
	for i, item := range plan {
		if item.Step == "" {
			return fmt.Errorf("plan step %d is empty", i)
		}
		switch item.Status {
		case PlanStatusPending, PlanStatusCompleted:
		case PlanStatusInProgress:
			inProgress++
		default:
			return fmt.Errorf("plan step %d has unknown status %q (want pending, in_progress or completed)", i, item.Status)
		}
	}
	if inProgress > 1 {
		return fmt.Errorf("at most one plan step can be in_progress, got %d", inProgress)
	}
	return nil
}

// UpdatePlanFunc records a plan update. The agent wires it to its checkpointed
// state; the tool only validates and forwards.
type UpdatePlanFunc func(ctx context.Context, explanation string, plan []PlanItem) error

var updatePlanSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"explanation": {
			"type": "string",
			"description": "Optional explanation for this plan update."
		},
		"plan": {
			"type": "array",
			"description": "The list of steps. At most one step can be in_progress at a time. An empty list clears the plan.",
			"items": {
				"type": "object",
				"properties": {
					"step": {"type": "string", "description": "Task step text."},
					"status": {"type": "string", "enum": ["pending", "in_progress", "completed"], "description": "Step status."}
				},
				"required": ["step", "status"],
				"additionalProperties": false
			}
		}
	},
	"required": ["plan"],
	"additionalProperties": false
}`)

type updatePlanTool struct {
	BaseTool
	update UpdatePlanFunc
}

// NewUpdatePlanTool creates the update_plan tool (codex parity): the model
// maintains a visible step plan across a multi-step task. The update func
// persists the plan (agent.WithPlanTool wires it to the agent's checkpointed
// state). Panics on a nil func: programmer error, mirroring
// NewCompressContextTool.
//
// The tool is deliberately NOT concurrency-safe: plan updates are ordered
// state transitions, and a concurrent batch would execute them in
// nondeterministic order (the state lock serializes the writes but not their
// sequence), leaving final state at odds with the emitted event order.
func NewUpdatePlanTool(update UpdatePlanFunc) Tool {
	if update == nil {
		panic("tool: NewUpdatePlanTool requires a non-nil update func")
	}
	return &updatePlanTool{
		BaseTool: BaseTool{
			ToolName:        "update_plan",
			ToolDescription: "Updates the task plan. Provide an optional explanation and a list of plan items, each with a step and status (pending, in_progress, completed). At most one step can be in_progress at a time. Use it to track multi-step work and to signal progress; an empty plan clears the tracker.",
			ToolSchema:      updatePlanSchema,
			// The plan is agent-internal bookkeeping, not an environment
			// mutation: read-only for permission purposes, so explore mode
			// does not gate planning. Not concurrency-safe: updates are
			// ordered state transitions (see the doc comment).
			ReadOnly: true,
		},
		update: update,
	}
}

func (t *updatePlanTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	raw, ok := args["plan"]
	if !ok {
		return NewErrorResponse(fmt.Errorf("plan is required")), nil
	}
	// Round-trip through JSON so nested maps decode into PlanItem fields.
	rawJSON, err := json.Marshal(raw)
	if err != nil {
		return NewErrorResponse(fmt.Errorf("invalid plan: %w", err)), nil
	}
	var plan []PlanItem
	if err := json.Unmarshal(rawJSON, &plan); err != nil {
		return NewErrorResponse(fmt.Errorf("invalid plan items: %w", err)), nil
	}
	explanation, _ := args["explanation"].(string)

	if err := ValidatePlanUpdate(plan); err != nil {
		return NewErrorResponse(err), nil
	}
	if err := t.update(ctx, explanation, plan); err != nil {
		return NewErrorResponse(fmt.Errorf("record plan: %w", err)), nil
	}
	resp := NewTextResponse("Plan updated")
	resp.Metadata = map[string]any{PlanMetadataKey: PlanMetadata{Explanation: explanation, Plan: plan}}
	return resp, nil
}
