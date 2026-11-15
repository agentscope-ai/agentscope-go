package permission

// Rule defines a permission policy for a specific tool or tool operation.
//
// The RuleContent field has tool-specific semantics:
//   - For "Bash"/"Shell": anchored glob ("prefix:*" for prefixes) matched
//     conjunctively against every subcommand of a compound command (leading
//     env assignments are stripped before matching)
//
// Static text matching has inherent blind spots a jail or sandbox must cover:
// wrapper commands (`sudo cmd`, `xargs cmd`, `sh -c cmd`), command names
// produced by substitution (`$(echo x)`), and aliases are not resolved. Rules
// are a policy layer over parsed command text, not a security boundary on
// their own.
//   - For "Write"/"Read"/"Edit": glob pattern matched against file paths
//   - For other tools: tool-specific filter, or empty to match all invocations
type Rule struct {
	ToolName    string             `json:"tool_name"`
	RuleContent string             `json:"rule_content,omitempty"`
	Behavior    PermissionBehavior `json:"behavior"`
	Source      string             `json:"source"`
}
