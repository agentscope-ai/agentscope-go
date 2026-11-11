package permission

import (
	"fmt"
	"sync"
)

// Engine evaluates tool execution requests against configured permission rules.
// Each PermissionMode has its own check method so mode policies are
// self-contained and readable in isolation.
//
// An Engine is safe for concurrent use: AddRule takes a write lock and the
// CheckPermission family takes a read lock over the rule maps.
type Engine struct {
	mu      sync.RWMutex
	Context *Context
	store   RuleStore
}

// NewEngine creates a permission engine with the given context.
func NewEngine(ctx *Context) *Engine {
	return &Engine{Context: ctx}
}

// AddRule adds a permission rule to the engine's context.
func (e *Engine) AddRule(rule Rule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch rule.Behavior {
	case BehaviorAllow:
		e.Context.AllowRules[rule.ToolName] = append(e.Context.AllowRules[rule.ToolName], rule)
	case BehaviorDeny:
		e.Context.DenyRules[rule.ToolName] = append(e.Context.DenyRules[rule.ToolName], rule)
	case BehaviorAsk:
		e.Context.AskRules[rule.ToolName] = append(e.Context.AskRules[rule.ToolName], rule)
	}
}

// SetRuleStore wires durable rule storage. AddRule keeps its in-memory-only
// behavior for rules loaded from configuration (they must not be written
// back); AddRulePersistent is the write-through path for rules learned from
// user approvals.
func (e *Engine) SetRuleStore(store RuleStore) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.store = store
}

// AddRulePersistent durably records a learned rule, then applies it in
// memory. Without a RuleStore it degrades to AddRule. On store failure the
// rule is NOT applied and the error is returned — the engine never silently
// keeps a rule the user believes is durable. Callers own the fallback policy:
// UnifiedAgent logs a warning and applies the rule in memory so the approved
// action still proceeds this session (fail-open for usability, and safe
// because the next session simply asks again).
func (e *Engine) AddRulePersistent(rule Rule) error {
	e.mu.RLock()
	store := e.store
	e.mu.RUnlock()
	if store == nil {
		e.AddRule(rule)
		return nil
	}
	if err := store.AppendRule(rule); err != nil {
		return err
	}
	e.AddRule(rule)
	return nil
}

// AddWorkingDirectory records dir as part of the permission scope. The agent
// layer uses it for user-granted workspace-jail escapes; the context is
// checkpointed with the agent state, so grants survive crash-resume within
// the session.
func (e *Engine) AddWorkingDirectory(dir WorkingDirectory) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Context.WorkingDirectories == nil {
		e.Context.WorkingDirectories = make(map[string]WorkingDirectory)
	}
	e.Context.WorkingDirectories[dir.Path] = dir
}

// GrantedDirectories returns the paths of all working directories in scope
// (any source), for injection into the file-tool path jail.
func (e *Engine) GrantedDirectories() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.Context.WorkingDirectories))
	for _, d := range e.Context.WorkingDirectories {
		out = append(out, d.Path)
	}
	return out
}

// SnapshotContext returns a deep copy of the engine's Context, safe to
// serialize or mutate without holding the engine lock.
func (e *Engine) SnapshotContext() *Context {
	e.mu.RLock()
	defer e.mu.RUnlock()
	cp := *e.Context
	cp.WorkingDirectories = make(map[string]WorkingDirectory, len(e.Context.WorkingDirectories))
	for k, v := range e.Context.WorkingDirectories {
		cp.WorkingDirectories[k] = v
	}
	copyRules := func(in map[string][]Rule) map[string][]Rule {
		out := make(map[string][]Rule, len(in))
		for k, v := range in {
			out[k] = append([]Rule(nil), v...)
		}
		return out
	}
	cp.AllowRules = copyRules(e.Context.AllowRules)
	cp.DenyRules = copyRules(e.Context.DenyRules)
	cp.AskRules = copyRules(e.Context.AskRules)
	return &cp
}

// MergeWorkingDirectories merges restored working directories (e.g. from a
// checkpoint) into the context without disturbing host-configured rules or
// the mode. Existing entries win: the host's session configuration is the
// authority for directories it declared itself.
func (e *Engine) MergeWorkingDirectories(dirs map[string]WorkingDirectory) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Context.WorkingDirectories == nil {
		e.Context.WorkingDirectories = make(map[string]WorkingDirectory, len(dirs))
	}
	for k, v := range dirs {
		if _, exists := e.Context.WorkingDirectories[k]; !exists {
			e.Context.WorkingDirectories[k] = v
		}
	}
}

// CheckPermission evaluates a tool execution request and returns a Decision.
func (e *Engine) CheckPermission(tool Checker, input map[string]any) (Decision, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.checkPermissionDispatch(tool, input)
}

// CheckPermissionInContext evaluates like CheckPermission but against pctx
// instead of the engine's stored Context. Callers use it to inject per-call
// facts (e.g. a workspace backend implies a POSIX target shell) without
// mutating shared engine state. The engine lock is still held, so rule maps
// shared with the stored Context cannot be mutated concurrently. A nil pctx
// falls back to the engine's Context.
func (e *Engine) CheckPermissionInContext(tool Checker, input map[string]any, pctx *Context) (Decision, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if pctx == nil {
		return e.checkPermissionDispatch(tool, input)
	}
	shadow := &Engine{Context: pctx}
	return shadow.checkPermissionDispatch(tool, input)
}

func (e *Engine) checkPermissionDispatch(tool Checker, input map[string]any) (Decision, error) {
	switch e.Context.Mode {
	case ModeDefault:
		return e.checkDefault(tool, input), nil
	case ModeExplore:
		return e.checkExplore(tool, input), nil
	case ModeAcceptEdits:
		return e.checkAcceptEdits(tool, input), nil
	case ModeBypass:
		return e.checkBypass(tool, input), nil
	case ModeDontAsk:
		return e.checkDontAsk(tool, input), nil
	default:
		return Decision{}, fmt.Errorf("permission: unknown mode %q", e.Context.Mode)
	}
}

// checkDefault implements ModeDefault:
//  1. Deny rules -> DENY
//  2. Ask rules -> ASK (with suggestions)
//  3. tool.CheckPermissions: ALLOW/DENY returned as-is; safety ASK (bypass-immune)
//     returned with suggestions; non-safety ASK/PASSTHROUGH -> continue
//  4. Allow rules -> ALLOW
//  5. Default -> ASK (with suggestions)
func (e *Engine) checkDefault(tool Checker, input map[string]any) Decision {
	if d := e.checkDenyRules(tool, input); d != nil {
		return *d
	}
	if d := e.checkAskRules(tool, input); d != nil {
		d.SuggestedRules = e.generateSuggestions(tool, input)
		return *d
	}

	td := tool.CheckPermissions(input, e.Context)
	// Only the engine may certify the default fallback branch.
	td.Origin = OriginUnknown
	if td.Behavior == BehaviorAllow || td.Behavior == BehaviorDeny {
		return td
	}
	if isSafetyAsk(&td) {
		td.SuggestedRules = e.generateSuggestions(tool, input)
		return td
	}

	if d := e.checkAllowRules(tool, input); d != nil {
		return *d
	}

	return Decision{
		Origin:         OriginDefaultAsk,
		Behavior:       BehaviorAsk,
		Message:        fmt.Sprintf("Permission required for %s", tool.Name()),
		DecisionReason: fmt.Sprintf("Mode: %s", e.Context.Mode),
		SuggestedRules: e.generateSuggestions(tool, input),
	}
}

// checkExplore implements ModeExplore (read-only):
//  1. Deny rules -> DENY
//  2. Ask rules -> ASK (with suggestions)
//  3. tool.CheckReadOnly: true -> ALLOW, false -> DENY
func (e *Engine) checkExplore(tool Checker, input map[string]any) Decision {
	if d := e.checkDenyRules(tool, input); d != nil {
		return *d
	}
	if d := e.checkAskRules(tool, input); d != nil {
		d.SuggestedRules = e.generateSuggestions(tool, input)
		return *d
	}

	if tool.CheckReadOnly(input) {
		return Decision{
			Behavior:       BehaviorAllow,
			Message:        fmt.Sprintf("Permission granted for %s (explore mode - read-only invocation)", tool.Name()),
			DecisionReason: "Explore mode allows read-only operations",
		}
	}
	return Decision{
		Behavior:       BehaviorDeny,
		Message:        fmt.Sprintf("Permission denied for %s (explore mode is read-only)", tool.Name()),
		DecisionReason: "Explore mode does not allow modifications",
	}
}

// checkAcceptEdits implements ModeAcceptEdits:
//  1. Deny rules -> DENY
//  2. Ask rules -> ASK (with suggestions)
//  3. tool.CheckReadOnly -> true -> ALLOW (fast path)
//  4. tool.CheckPermissions: ALLOW/DENY as-is; safety ASK -> with suggestions;
//     non-safety ASK/PASSTHROUGH -> continue
//  5. Allow rules -> ALLOW
//  6. Default -> ASK (with suggestions)
func (e *Engine) checkAcceptEdits(tool Checker, input map[string]any) Decision {
	if d := e.checkDenyRules(tool, input); d != nil {
		return *d
	}
	if d := e.checkAskRules(tool, input); d != nil {
		d.SuggestedRules = e.generateSuggestions(tool, input)
		return *d
	}

	if tool.CheckReadOnly(input) {
		return Decision{
			Behavior:       BehaviorAllow,
			Message:        fmt.Sprintf("Permission granted for %s (accept edits mode - read-only invocation)", tool.Name()),
			DecisionReason: "Accept edits mode allows read-only operations",
		}
	}

	td := tool.CheckPermissions(input, e.Context)
	// Only the engine may certify the default fallback branch.
	td.Origin = OriginUnknown
	if td.Behavior == BehaviorAllow || td.Behavior == BehaviorDeny {
		return td
	}
	if isSafetyAsk(&td) {
		td.SuggestedRules = e.generateSuggestions(tool, input)
		return td
	}

	if d := e.checkAllowRules(tool, input); d != nil {
		return *d
	}

	return Decision{
		Behavior:       BehaviorAsk,
		Message:        fmt.Sprintf("Permission required for %s", tool.Name()),
		DecisionReason: fmt.Sprintf("Mode: %s", e.Context.Mode),
		SuggestedRules: e.generateSuggestions(tool, input),
	}
}

// checkBypass implements ModeBypass:
//  1. Deny rules -> DENY
//  2. Ask rules -> ASK (honors explicit user intent)
//  3. tool.CheckPermissions: ALLOW/DENY as-is; any ASK (including bypass-immune) falls through
//  4. Allow rules -> ALLOW
//  5. Fallback -> ALLOW
func (e *Engine) checkBypass(tool Checker, input map[string]any) Decision {
	if d := e.checkDenyRules(tool, input); d != nil {
		return *d
	}
	if d := e.checkAskRules(tool, input); d != nil {
		d.SuggestedRules = e.generateSuggestions(tool, input)
		return *d
	}

	td := tool.CheckPermissions(input, e.Context)
	// Only the engine may certify the default fallback branch.
	td.Origin = OriginUnknown
	if td.Behavior == BehaviorAllow || td.Behavior == BehaviorDeny {
		return td
	}

	if d := e.checkAllowRules(tool, input); d != nil {
		return *d
	}

	return Decision{
		Behavior:       BehaviorAllow,
		Message:        fmt.Sprintf("Permission granted for %s (bypass mode)", tool.Name()),
		DecisionReason: "Bypass mode allows all operations",
	}
}

// checkDontAsk implements ModeDontAsk:
//  1. Deny rules -> DENY
//  2. Ask rules -> DENY (converted)
//  3. tool.CheckPermissions: ALLOW/DENY as-is; safety ASK -> DENY; non-safety ASK/PASSTHROUGH -> continue
//  4. Allow rules -> ALLOW
//  5. Default -> DENY
func (e *Engine) checkDontAsk(tool Checker, input map[string]any) Decision {
	if d := e.checkDenyRules(tool, input); d != nil {
		return *d
	}
	if d := e.checkAskRules(tool, input); d != nil {
		d.SuggestedRules = e.generateSuggestions(tool, input)
		return convertAskToDeny(tool, d)
	}

	td := tool.CheckPermissions(input, e.Context)
	// Only the engine may certify the default fallback branch.
	td.Origin = OriginUnknown
	if td.Behavior == BehaviorAllow || td.Behavior == BehaviorDeny {
		return td
	}
	if isSafetyAsk(&td) {
		td.SuggestedRules = e.generateSuggestions(tool, input)
		return convertAskToDeny(tool, &td)
	}

	if d := e.checkAllowRules(tool, input); d != nil {
		return *d
	}

	return Decision{
		Behavior:       BehaviorDeny,
		Message:        fmt.Sprintf("Permission denied for %s (dont_ask mode - user not available)", tool.Name()),
		DecisionReason: "User is not available to answer permission prompts",
	}
}

func (e *Engine) checkDenyRules(tool Checker, input map[string]any) *Decision {
	if rules := e.Context.DenyRules[tool.Name()]; len(rules) > 0 {
		if cm, ok := tool.(CompoundRuleMatcher); ok {
			if cm.MatchAnySubcommand(ruleContents(rules), input) {
				return &Decision{
					Behavior:       BehaviorDeny,
					Message:        fmt.Sprintf("Permission to use %s has been denied", tool.Name()),
					DecisionReason: "Rule: compound subcommand match",
				}
			}
			return nil
		}
	}
	for _, rule := range e.Context.DenyRules[tool.Name()] {
		if e.ruleMatches(tool, rule, input) {
			return &Decision{
				Behavior:       BehaviorDeny,
				Message:        fmt.Sprintf("Permission to use %s has been denied", tool.Name()),
				DecisionReason: fmt.Sprintf("Rule: %s", rule.RuleContent),
			}
		}
	}
	return nil
}

func (e *Engine) checkAskRules(tool Checker, input map[string]any) *Decision {
	if rules := e.Context.AskRules[tool.Name()]; len(rules) > 0 {
		if cm, ok := tool.(CompoundRuleMatcher); ok {
			if cm.MatchAnySubcommand(ruleContents(rules), input) {
				return &Decision{
					Behavior:       BehaviorAsk,
					Message:        fmt.Sprintf("Permission required for %s", tool.Name()),
					DecisionReason: "Rule: compound subcommand match",
				}
			}
			return nil
		}
	}
	for _, rule := range e.Context.AskRules[tool.Name()] {
		if e.ruleMatches(tool, rule, input) {
			return &Decision{
				Behavior:       BehaviorAsk,
				Message:        fmt.Sprintf("Permission required for %s", tool.Name()),
				DecisionReason: fmt.Sprintf("Rule: %s", rule.RuleContent),
			}
		}
	}
	return nil
}

func (e *Engine) checkAllowRules(tool Checker, input map[string]any) *Decision {
	if rules := e.Context.AllowRules[tool.Name()]; len(rules) > 0 {
		if cm, ok := tool.(CompoundRuleMatcher); ok {
			if cm.MatchAllSubcommands(ruleContents(rules), input) {
				return &Decision{
					Behavior:     BehaviorAllow,
					Message:      fmt.Sprintf("Permission granted for %s", tool.Name()),
					UpdatedInput: input,
				}
			}
			return nil
		}
	}
	for _, rule := range e.Context.AllowRules[tool.Name()] {
		if e.ruleMatches(tool, rule, input) {
			return &Decision{
				Behavior:     BehaviorAllow,
				Message:      fmt.Sprintf("Permission granted for %s", tool.Name()),
				UpdatedInput: input,
			}
		}
	}
	return nil
}

func (e *Engine) ruleMatches(tool Checker, rule Rule, input map[string]any) bool {
	if rule.RuleContent == "" {
		return true
	}
	return tool.MatchRule(rule.RuleContent, input)
}

// ruleContents flattens rules to their content patterns for
// CompoundRuleMatcher tools.
func ruleContents(rules []Rule) []string {
	contents := make([]string, len(rules))
	for i, r := range rules {
		contents[i] = r.RuleContent
	}
	return contents
}

func (e *Engine) generateSuggestions(tool Checker, input map[string]any) []Rule {
	return tool.GenerateSuggestions(input)
}

func isSafetyAsk(d *Decision) bool {
	return d.Behavior == BehaviorAsk && d.BypassImmune
}

func convertAskToDeny(tool Checker, ask *Decision) Decision {
	return Decision{
		Behavior: BehaviorDeny,
		Message: fmt.Sprintf(
			"Permission denied for %s (dont_ask mode - ASK converted to DENY, user not available)",
			tool.Name(),
		),
		DecisionReason: fmt.Sprintf("DONT_ASK mode converted ASK to DENY. Original reason: %s", ask.DecisionReason),
		SuggestedRules: ask.SuggestedRules,
	}
}
