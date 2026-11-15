package permission

// PermissionMode controls how the system handles tool execution requests.
type PermissionMode string

const (
	// ModeDefault requires explicit permission per action unless an allow rule
	// matches or the tool's CheckPermissions returns Allow.
	ModeDefault PermissionMode = "default"

	// ModeAcceptEdits auto-allows file edits within working directories and
	// read-only operations; other operations follow normal rules.
	ModeAcceptEdits PermissionMode = "accept_edits"

	// ModeExplore is read-only mode: modifications are denied, reads are allowed.
	ModeExplore PermissionMode = "explore"

	// ModeBypass skips all safety checks except explicit deny/ask rules and
	// tool-emitted Deny. Use only in sandboxed environments.
	ModeBypass PermissionMode = "bypass"

	// ModeDontAsk converts every Ask decision to Deny. Safe for unattended
	// execution when no user is available to answer prompts.
	ModeDontAsk PermissionMode = "dont_ask"
)

// PermissionBehavior is the outcome behavior of a permission check.
type PermissionBehavior string

const (
	// BehaviorAllow permits the operation.
	BehaviorAllow PermissionBehavior = "allow"

	// BehaviorDeny rejects the operation.
	BehaviorDeny PermissionBehavior = "deny"

	// BehaviorAsk requires the user to confirm the operation.
	BehaviorAsk PermissionBehavior = "ask"

	// BehaviorPassthrough defers the decision to the permission engine's
	// remaining rule evaluation.
	BehaviorPassthrough PermissionBehavior = "passthrough"
)
