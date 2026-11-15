package permission

import "strings"

// GrantRuleToolName is the reserved ToolName of suggested rules that ask the
// user to widen the workspace jail with an additional directory. Grant rules
// are never evaluated as permission rules; the agent layer intercepts them
// and records the directory in Context.WorkingDirectories instead.
const GrantRuleToolName = "workspace"

// grantDirPrefix prefixes RuleContent of a directory-grant rule.
const grantDirPrefix = "grant:dir:"

// NewGrantDirRule builds the suggested rule asking to grant the agent access
// to dir beyond its workspace root. The rule is session-scoped: it is not
// persisted through RuleStore (the settings format has no grants section),
// matching how sandbox escalations are handled per session.
func NewGrantDirRule(dir string) Rule {
	return Rule{
		ToolName:    GrantRuleToolName,
		RuleContent: grantDirPrefix + dir,
		Behavior:    BehaviorAllow,
		Source:      "grant",
	}
}

// ParseGrantDirRule extracts the directory from a grant rule built by
// NewGrantDirRule. The second return value reports whether r is one.
func ParseGrantDirRule(r Rule) (string, bool) {
	if r.ToolName != GrantRuleToolName {
		return "", false
	}
	dir, ok := strings.CutPrefix(r.RuleContent, grantDirPrefix)
	if !ok || dir == "" {
		return "", false
	}
	return dir, true
}
