package agent

import (
	"fmt"
	"strings"
)

// BuildStateContext renders a human-readable summary of the current agent state
// including active tasks, tool groups, and permission mode.
func BuildStateContext(state *AgentState) string {
	if state == nil {
		return ""
	}

	var sb strings.Builder

	// Session info
	if state.SessionID != "" {
		sb.WriteString(fmt.Sprintf("Session: %s\n", state.SessionID))
	}
	sb.WriteString(fmt.Sprintf("Iteration: %d\n", state.CurIter))

	// Permission context
	if state.PermissionCtx != nil {
		sb.WriteString(fmt.Sprintf("Permission Mode: %s\n", state.PermissionCtx.Mode))
	}

	// Tool context
	if state.ToolCtx != nil && len(state.ToolCtx.ActivatedGroups) > 0 {
		sb.WriteString(fmt.Sprintf("Active Tool Groups: %s\n", strings.Join(state.ToolCtx.ActivatedGroups, ", ")))
	}

	// Tasks context
	if state.TasksCtx != nil && len(state.TasksCtx.Tasks) > 0 {
		sb.WriteString("Active Tasks:\n")
		for i := range state.TasksCtx.Tasks {
			t := &state.TasksCtx.Tasks[i]
			line := fmt.Sprintf("  - [%s] %s", t.State, t.Subject)
			if t.Owner != "" {
				line += fmt.Sprintf(" (owner: %s)", t.Owner)
			}
			sb.WriteString(line + "\n")
		}
	}

	return sb.String()
}

// InjectStateAwareness appends the rendered agent state context block to the
// system prompt, wrapped in XML-style tags. If state is nil or produces no
// content, the original prompt is returned unchanged.
func InjectStateAwareness(systemPrompt string, state *AgentState) string {
	stateBlock := BuildStateContext(state)
	if stateBlock == "" {
		return systemPrompt
	}

	return systemPrompt + "\n\n<agent_state>\n" + strings.TrimRight(stateBlock, "\n") + "\n</agent_state>"
}
