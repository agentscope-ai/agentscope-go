package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

var resetToolsSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"activate": {
			"type": "array",
			"items": {"type": "string"},
			"description": "Tool group names to activate"
		},
		"deactivate": {
			"type": "array",
			"items": {"type": "string"},
			"description": "Tool group names to deactivate"
		}
	}
}`)

type resetToolsTool struct {
	BaseTool
	toolkit *Toolkit
}

func (t *resetToolsTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	if t.toolkit == nil {
		return NewErrorResponse(fmt.Errorf("no toolkit reference")), nil
	}

	var activated, deactivated []string
	for _, key := range []string{"activate", "deactivate"} {
		v, ok := args[key]
		if !ok {
			continue
		}
		names, err := toStringSliceStrict(v)
		if err != nil {
			return NewErrorResponse(fmt.Errorf(
				"Invalid arguments: the argument '%s' should be an array of tool group names.", key)), nil
		}
		if key == "activate" {
			activated = names
		} else {
			deactivated = names
		}
	}

	// Validate every group name BEFORE mutating any state (upstream fix #2302):
	// an invalid name must not clear or change previously activated groups.
	var available []string
	for _, name := range t.toolkit.GroupNames() {
		if name != "basic" {
			available = append(available, name)
		}
	}
	var invalid []string
	for _, name := range append(append([]string{}, activated...), deactivated...) {
		if name == "basic" || !t.toolkit.HasGroup(name) {
			invalid = append(invalid, name)
		}
	}
	if len(invalid) > 0 {
		availableStr := strings.Join(available, ", ")
		if availableStr == "" {
			availableStr = "(none)"
		}
		return NewErrorResponse(fmt.Errorf(
			"Invalid group name(s): %s. The current available groups are: %s",
			strings.Join(invalid, ", "), availableStr)), nil
	}

	for _, name := range activated {
		t.toolkit.ActivateGroup(name)
	}
	for _, name := range deactivated {
		t.toolkit.DeactivateGroup(name)
	}

	var parts []string
	if len(activated) > 0 {
		parts = append(parts, fmt.Sprintf("Activated: %s", strings.Join(activated, ", ")))
	}
	if len(deactivated) > 0 {
		parts = append(parts, fmt.Sprintf("Deactivated: %s", strings.Join(deactivated, ", ")))
	}
	if len(parts) == 0 {
		return NewTextResponse("No changes made."), nil
	}
	return NewTextResponse(strings.Join(parts, ". ")), nil
}

// toStringSliceStrict converts an array value to []string, rejecting
// non-string elements outright. ResetTools validation must not silently drop
// elements (evaluator M7): `activate: [1]` is an invalid argument, not a
// no-op.
func toStringSliceStrict(v any) ([]string, error) {
	switch arr := v.(type) {
	case []any:
		out := make([]string, 0, len(arr))
		for i, item := range arr {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("element %d is not a string", i)
			}
			out = append(out, s)
		}
		return out, nil
	case []string:
		return arr, nil
	default:
		return nil, fmt.Errorf("not an array")
	}
}

func toStringSlice(v any) []string {
	switch arr := v.(type) {
	case []any:
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return arr
	default:
		return nil
	}
}

// ResetToolsTool returns a meta-tool that dynamically activates/deactivates tool groups.
// It needs a reference to the Toolkit it manages.
func ResetToolsTool(tk *Toolkit) Tool {
	return &resetToolsTool{
		BaseTool: BaseTool{
			ToolName:        "ResetTools",
			ToolDescription: "Activate or deactivate tool groups to change available tools. Use when you need different capabilities.",
			ToolSchema:      resetToolsSchema,
		},
		toolkit: tk,
	}
}
