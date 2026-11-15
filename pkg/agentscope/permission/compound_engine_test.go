package permission

import (
	"strings"
	"testing"
)

// compoundBash emulates the Bash tool's compound matching for engine tests.
type compoundBash struct {
	name string
}

func (compoundBash) CheckPermissions(map[string]any, *Context) Decision {
	return Decision{Behavior: BehaviorPassthrough}
}
func (compoundBash) CheckReadOnly(map[string]any) bool { return false }
func (c compoundBash) Name() string                    { return c.name }
func (c compoundBash) MatchRule(rule string, input map[string]any) bool {
	cmd, _ := input["command"].(string)
	return strings.HasPrefix(cmd, strings.TrimSuffix(rule, ":*"))
}
func (c compoundBash) GenerateSuggestions(map[string]any) []Rule { return nil }

// subcommands splits on " && " for the test double.
func subcommands(input map[string]any) []string {
	cmd, _ := input["command"].(string)
	return strings.Split(cmd, " && ")
}

func (c compoundBash) MatchAllSubcommands(rules []string, input map[string]any) bool {
	for _, sub := range subcommands(input) {
		ok := false
		for _, r := range rules {
			if strings.HasPrefix(sub, strings.TrimSuffix(r, ":*")) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func (c compoundBash) MatchAnySubcommand(rules []string, input map[string]any) bool {
	for _, sub := range subcommands(input) {
		for _, r := range rules {
			if strings.HasPrefix(sub, strings.TrimSuffix(r, ":*")) {
				return true
			}
		}
	}
	return false
}

func TestEngineCompoundAllowRequiresAllSubcommands(t *testing.T) {
	ctx := NewContext(ModeDefault)
	ctx.AllowRules["Bash"] = []Rule{{ToolName: "Bash", RuleContent: "git status:*", Behavior: BehaviorAllow}}
	e := NewEngine(ctx)
	tool := compoundBash{name: "Bash"}

	d, err := e.CheckPermission(tool, map[string]any{"command": "git status -s"})
	if err != nil || d.Behavior != BehaviorAllow {
		t.Fatalf("covered prefix: %+v err=%v", d, err)
	}
	d, _ = e.CheckPermission(tool, map[string]any{"command": "git status && wipe"})
	if d.Behavior == BehaviorAllow {
		t.Fatalf("uncovered tail must not be allowed: %+v", d)
	}
}

func TestEngineCompoundDenyCatchesHiddenTail(t *testing.T) {
	ctx := NewContext(ModeDefault)
	ctx.DenyRules["Bash"] = []Rule{{ToolName: "Bash", RuleContent: "wipe:*", Behavior: BehaviorDeny}}
	e := NewEngine(ctx)
	tool := compoundBash{name: "Bash"}

	d, _ := e.CheckPermission(tool, map[string]any{"command": "git status && wipe all"})
	if d.Behavior != BehaviorDeny {
		t.Fatalf("denied tail behind innocent head: %+v", d)
	}
	d, _ = e.CheckPermission(tool, map[string]any{"command": "git status"})
	if d.Behavior == BehaviorDeny {
		t.Fatalf("innocent command denied: %+v", d)
	}
}
