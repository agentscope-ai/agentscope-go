// Package protocol defines wire-level constants and types for the agent loop
// state machine, approval policies, and permission profiles. It is a leaf
// package with zero internal dependencies.
package protocol

// LoopState represents a state in the agent loop state machine.
type LoopState int

const (
	StateReason  LoopState = iota // Model call / reasoning
	StateInspect                  // Inspect model response for tool calls
	StateAct                      // Execute tool calls
	StateWait                     // Waiting for external input (HITL)
	StateExit                     // Loop terminated
)

var loopStateNames = [...]string{
	"reason",
	"inspect",
	"act",
	"wait",
	"exit",
}

func (s LoopState) String() string {
	if int(s) < len(loopStateNames) {
		return loopStateNames[s]
	}
	return "unknown"
}

// ApprovalPolicy controls when tool execution requires user approval.
type ApprovalPolicy string

const (
	// ApprovalDefault uses the engine's default approval behavior.
	ApprovalDefault ApprovalPolicy = "default"
	// ApprovalUnlessSafe requires approval unless the tool is marked safe.
	ApprovalUnlessSafe ApprovalPolicy = "unless_safe"
	// ApprovalNever never requires user approval for tool execution.
	ApprovalNever ApprovalPolicy = "never"
	// ApprovalAlways always requires user approval before executing any tool.
	ApprovalAlways ApprovalPolicy = "always"
)

// PermissionProfile defines the permission mode for a session.
type PermissionProfile string

const (
	// PermDefault applies the standard permission checks.
	PermDefault PermissionProfile = "default"
	// PermAcceptEdits auto-approves file-editing tools but prompts for others.
	PermAcceptEdits PermissionProfile = "accept_edits"
	// PermExplore restricts the session to read-only tools.
	PermExplore PermissionProfile = "explore"
	// PermBypass skips all permission checks (use with caution).
	PermBypass PermissionProfile = "bypass"
	// PermDontAsk auto-approves every tool without prompting.
	PermDontAsk PermissionProfile = "dont_ask"
)
