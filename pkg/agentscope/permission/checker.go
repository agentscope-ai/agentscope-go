package permission

// Checker is the interface tools implement to participate in permission checks.
// The Tool interface in pkg/agentscope/tool embeds this.
type Checker interface {
	// Name returns the tool's name (used for rule matching by tool_name).
	Name() string

	// CheckPermissions evaluates whether the given input should be allowed,
	// denied, or requires user confirmation. Return a Passthrough decision to
	// defer to the engine's rule evaluation.
	CheckPermissions(input map[string]any, ctx *Context) Decision

	// CheckReadOnly reports whether this specific invocation is read-only.
	// Default implementations return the tool's static IsReadOnly value;
	// tools like Bash should inspect the input (e.g. "ls" is read-only).
	CheckReadOnly(input map[string]any) bool

	// MatchRule checks whether a rule's content pattern matches the tool input.
	// Empty ruleContent matches all invocations. Tools override this for
	// fine-grained matching (substring for Bash, glob for file tools).
	MatchRule(ruleContent string, input map[string]any) bool

	// GenerateSuggestions produces permission rules the user could add to
	// avoid future confirmations for similar tool invocations.
	GenerateSuggestions(input map[string]any) []Rule
}

// CompoundRuleMatcher is an optional Checker extension for tools whose input
// can carry compound operations (e.g. shell commands joined with &&, pipes or
// command substitutions). The engine consults it with the whole rule set for
// the tool instead of one rule at a time:
//
//   - Allow rules: the invocation matches only when EVERY subcommand matches
//     at least one rule, so "allowed-prefix && anything" cannot ride on the
//     allowed prefix.
//   - Deny/ask rules: the invocation matches when ANY subcommand matches ANY
//     rule, so a denied tail cannot hide behind an innocent head.
//
// Tools without compound semantics implement MatchRule only and keep per-rule
// whole-input matching.
type CompoundRuleMatcher interface {
	MatchAllSubcommands(rules []string, input map[string]any) bool
	MatchAnySubcommand(rules []string, input map[string]any) bool
}
