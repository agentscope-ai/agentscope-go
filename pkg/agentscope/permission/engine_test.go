package permission

import (
	"testing"
)

type mockTool struct {
	name       string
	readOnly   bool
	checkFn    func(map[string]any, *Context) Decision
	matchFn    func(string, map[string]any) bool
	suggestFn  func(map[string]any) []Rule
	readOnlyFn func(map[string]any) bool
}

func (m *mockTool) Name() string { return m.name }

func (m *mockTool) CheckPermissions(input map[string]any, ctx *Context) Decision {
	if m.checkFn != nil {
		return m.checkFn(input, ctx)
	}
	return Decision{Behavior: BehaviorPassthrough}
}

func (m *mockTool) CheckReadOnly(input map[string]any) bool {
	if m.readOnlyFn != nil {
		return m.readOnlyFn(input)
	}
	return m.readOnly
}

func (m *mockTool) MatchRule(ruleContent string, input map[string]any) bool {
	if m.matchFn != nil {
		return m.matchFn(ruleContent, input)
	}
	return ruleContent == ""
}

func (m *mockTool) GenerateSuggestions(input map[string]any) []Rule {
	if m.suggestFn != nil {
		return m.suggestFn(input)
	}
	return []Rule{{ToolName: m.name, Behavior: BehaviorAllow, Source: "suggested"}}
}

func newTool(name string) *mockTool {
	return &mockTool{name: name}
}

func newReadOnlyTool(name string) *mockTool {
	return &mockTool{name: name, readOnly: true}
}

func TestDefaultMode_NoRules_AsksUser(t *testing.T) {
	ctx := NewContext(ModeDefault)
	engine := NewEngine(ctx)

	d, err := engine.CheckPermission(newTool("Bash"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Behavior != BehaviorAsk {
		t.Fatalf("expected Ask, got %s", d.Behavior)
	}
}

func TestDefaultMode_DenyRuleMatches(t *testing.T) {
	ctx := NewContext(ModeDefault)
	engine := NewEngine(ctx)
	engine.AddRule(Rule{ToolName: "Bash", Behavior: BehaviorDeny, Source: "test"})

	d, _ := engine.CheckPermission(newTool("Bash"), nil)
	if d.Behavior != BehaviorDeny {
		t.Fatalf("expected Deny, got %s", d.Behavior)
	}
}

func TestDefaultMode_AllowRuleMatches(t *testing.T) {
	ctx := NewContext(ModeDefault)
	engine := NewEngine(ctx)
	engine.AddRule(Rule{ToolName: "Bash", Behavior: BehaviorAllow, Source: "test"})

	d, _ := engine.CheckPermission(newTool("Bash"), nil)
	if d.Behavior != BehaviorAllow {
		t.Fatalf("expected Allow, got %s", d.Behavior)
	}
}

func TestDefaultMode_DenyTakesPriorityOverAllow(t *testing.T) {
	ctx := NewContext(ModeDefault)
	engine := NewEngine(ctx)
	engine.AddRule(Rule{ToolName: "Bash", Behavior: BehaviorAllow, Source: "test"})
	engine.AddRule(Rule{ToolName: "Bash", Behavior: BehaviorDeny, Source: "test"})

	d, _ := engine.CheckPermission(newTool("Bash"), nil)
	if d.Behavior != BehaviorDeny {
		t.Fatalf("expected Deny, got %s", d.Behavior)
	}
}

func TestDefaultMode_ToolAllowOverridesDefault(t *testing.T) {
	tool := &mockTool{
		name: "Bash",
		checkFn: func(map[string]any, *Context) Decision {
			return Decision{Behavior: BehaviorAllow, Message: "read-only command"}
		},
	}
	ctx := NewContext(ModeDefault)
	engine := NewEngine(ctx)

	d, _ := engine.CheckPermission(tool, nil)
	if d.Behavior != BehaviorAllow {
		t.Fatalf("expected Allow, got %s", d.Behavior)
	}
}

func TestDefaultMode_SafetyAskNotOverriddenByAllowRules(t *testing.T) {
	tool := &mockTool{
		name: "Bash",
		checkFn: func(map[string]any, *Context) Decision {
			return Decision{
				Behavior:     BehaviorAsk,
				Message:      "dangerous operation",
				BypassImmune: true,
			}
		},
	}
	ctx := NewContext(ModeDefault)
	engine := NewEngine(ctx)
	engine.AddRule(Rule{ToolName: "Bash", Behavior: BehaviorAllow, Source: "test"})

	d, _ := engine.CheckPermission(tool, nil)
	if d.Behavior != BehaviorAsk {
		t.Fatalf("expected Ask (safety), got %s", d.Behavior)
	}
}

func TestDefaultMode_AskRuleProducesSuggestions(t *testing.T) {
	ctx := NewContext(ModeDefault)
	engine := NewEngine(ctx)
	engine.AddRule(Rule{ToolName: "Bash", Behavior: BehaviorAsk, Source: "test"})

	d, _ := engine.CheckPermission(newTool("Bash"), nil)
	if d.Behavior != BehaviorAsk {
		t.Fatalf("expected Ask, got %s", d.Behavior)
	}
	if len(d.SuggestedRules) == 0 {
		t.Fatal("expected suggested rules")
	}
}

func TestExploreMode_ReadOnlyAllowed(t *testing.T) {
	ctx := NewContext(ModeExplore)
	engine := NewEngine(ctx)

	d, _ := engine.CheckPermission(newReadOnlyTool("Read"), nil)
	if d.Behavior != BehaviorAllow {
		t.Fatalf("expected Allow, got %s", d.Behavior)
	}
}

func TestExploreMode_WriteDenied(t *testing.T) {
	ctx := NewContext(ModeExplore)
	engine := NewEngine(ctx)

	d, _ := engine.CheckPermission(newTool("Write"), nil)
	if d.Behavior != BehaviorDeny {
		t.Fatalf("expected Deny, got %s", d.Behavior)
	}
}

func TestExploreMode_DenyRuleTakesPriority(t *testing.T) {
	ctx := NewContext(ModeExplore)
	engine := NewEngine(ctx)
	engine.AddRule(Rule{ToolName: "Read", Behavior: BehaviorDeny, Source: "test"})

	d, _ := engine.CheckPermission(newReadOnlyTool("Read"), nil)
	if d.Behavior != BehaviorDeny {
		t.Fatalf("expected Deny, got %s", d.Behavior)
	}
}

func TestExploreMode_InputAwareReadOnly(t *testing.T) {
	bash := &mockTool{
		name: "Bash",
		readOnlyFn: func(input map[string]any) bool {
			cmd, _ := input["command"].(string)
			return cmd == "ls"
		},
	}
	ctx := NewContext(ModeExplore)
	engine := NewEngine(ctx)

	d, _ := engine.CheckPermission(bash, map[string]any{"command": "ls"})
	if d.Behavior != BehaviorAllow {
		t.Fatalf("expected Allow for 'ls', got %s", d.Behavior)
	}

	d, _ = engine.CheckPermission(bash, map[string]any{"command": "rm -rf /"})
	if d.Behavior != BehaviorDeny {
		t.Fatalf("expected Deny for 'rm -rf /', got %s", d.Behavior)
	}
}

func TestAcceptEditsMode_ReadOnlyFastPath(t *testing.T) {
	ctx := NewContext(ModeAcceptEdits)
	engine := NewEngine(ctx)

	d, _ := engine.CheckPermission(newReadOnlyTool("Grep"), nil)
	if d.Behavior != BehaviorAllow {
		t.Fatalf("expected Allow, got %s", d.Behavior)
	}
}

func TestAcceptEditsMode_ToolAllowForWorkingDir(t *testing.T) {
	tool := &mockTool{
		name: "Write",
		checkFn: func(input map[string]any, ctx *Context) Decision {
			return Decision{Behavior: BehaviorAllow, Message: "in working dir"}
		},
	}
	ctx := NewContext(ModeAcceptEdits)
	engine := NewEngine(ctx)

	d, _ := engine.CheckPermission(tool, map[string]any{"file_path": "/project/src/main.go"})
	if d.Behavior != BehaviorAllow {
		t.Fatalf("expected Allow, got %s", d.Behavior)
	}
}

func TestAcceptEditsMode_SafetyAskNotOverridden(t *testing.T) {
	tool := &mockTool{
		name: "Bash",
		checkFn: func(map[string]any, *Context) Decision {
			return Decision{Behavior: BehaviorAsk, BypassImmune: true, Message: "danger"}
		},
	}
	ctx := NewContext(ModeAcceptEdits)
	engine := NewEngine(ctx)
	engine.AddRule(Rule{ToolName: "Bash", Behavior: BehaviorAllow, Source: "test"})

	d, _ := engine.CheckPermission(tool, nil)
	if d.Behavior != BehaviorAsk {
		t.Fatalf("expected Ask (safety), got %s", d.Behavior)
	}
}

func TestBypassMode_AllowsEverything(t *testing.T) {
	ctx := NewContext(ModeBypass)
	engine := NewEngine(ctx)

	d, _ := engine.CheckPermission(newTool("Bash"), nil)
	if d.Behavior != BehaviorAllow {
		t.Fatalf("expected Allow, got %s", d.Behavior)
	}
}

func TestBypassMode_DenyRuleStillWorks(t *testing.T) {
	ctx := NewContext(ModeBypass)
	engine := NewEngine(ctx)
	engine.AddRule(Rule{ToolName: "Bash", Behavior: BehaviorDeny, Source: "test"})

	d, _ := engine.CheckPermission(newTool("Bash"), nil)
	if d.Behavior != BehaviorDeny {
		t.Fatalf("expected Deny, got %s", d.Behavior)
	}
}

func TestBypassMode_SafetyAskIgnored(t *testing.T) {
	tool := &mockTool{
		name: "Bash",
		checkFn: func(map[string]any, *Context) Decision {
			return Decision{Behavior: BehaviorAsk, BypassImmune: true, Message: "danger"}
		},
	}
	ctx := NewContext(ModeBypass)
	engine := NewEngine(ctx)

	d, _ := engine.CheckPermission(tool, nil)
	if d.Behavior != BehaviorAllow {
		t.Fatalf("expected Allow (bypass ignores safety ask), got %s", d.Behavior)
	}
}

func TestBypassMode_AskRuleHonored(t *testing.T) {
	ctx := NewContext(ModeBypass)
	engine := NewEngine(ctx)
	engine.AddRule(Rule{ToolName: "Bash", Behavior: BehaviorAsk, Source: "test"})

	d, _ := engine.CheckPermission(newTool("Bash"), nil)
	if d.Behavior != BehaviorAsk {
		t.Fatalf("expected Ask (explicit user rule), got %s", d.Behavior)
	}
}

func TestDontAskMode_DefaultDeny(t *testing.T) {
	ctx := NewContext(ModeDontAsk)
	engine := NewEngine(ctx)

	d, _ := engine.CheckPermission(newTool("Bash"), nil)
	if d.Behavior != BehaviorDeny {
		t.Fatalf("expected Deny, got %s", d.Behavior)
	}
}

func TestDontAskMode_AskConvertedToDeny(t *testing.T) {
	ctx := NewContext(ModeDontAsk)
	engine := NewEngine(ctx)
	engine.AddRule(Rule{ToolName: "Bash", Behavior: BehaviorAsk, Source: "test"})

	d, _ := engine.CheckPermission(newTool("Bash"), nil)
	if d.Behavior != BehaviorDeny {
		t.Fatalf("expected Deny (ASK converted), got %s", d.Behavior)
	}
}

func TestDontAskMode_SafetyAskConvertedToDeny(t *testing.T) {
	tool := &mockTool{
		name: "Bash",
		checkFn: func(map[string]any, *Context) Decision {
			return Decision{Behavior: BehaviorAsk, BypassImmune: true, Message: "danger"}
		},
	}
	ctx := NewContext(ModeDontAsk)
	engine := NewEngine(ctx)

	d, _ := engine.CheckPermission(tool, nil)
	if d.Behavior != BehaviorDeny {
		t.Fatalf("expected Deny (safety ASK converted), got %s", d.Behavior)
	}
}

func TestDontAskMode_AllowRuleWorks(t *testing.T) {
	ctx := NewContext(ModeDontAsk)
	engine := NewEngine(ctx)
	engine.AddRule(Rule{ToolName: "Bash", Behavior: BehaviorAllow, Source: "test"})

	d, _ := engine.CheckPermission(newTool("Bash"), nil)
	if d.Behavior != BehaviorAllow {
		t.Fatalf("expected Allow, got %s", d.Behavior)
	}
}

func TestDontAskMode_ToolAllowReturned(t *testing.T) {
	tool := &mockTool{
		name: "Bash",
		checkFn: func(map[string]any, *Context) Decision {
			return Decision{Behavior: BehaviorAllow, Message: "read-only"}
		},
	}
	ctx := NewContext(ModeDontAsk)
	engine := NewEngine(ctx)

	d, _ := engine.CheckPermission(tool, nil)
	if d.Behavior != BehaviorAllow {
		t.Fatalf("expected Allow, got %s", d.Behavior)
	}
}

func TestDontAskMode_NeverReturnsAsk(t *testing.T) {
	for _, mode := range []PermissionMode{ModeDontAsk} {
		ctx := NewContext(mode)
		engine := NewEngine(ctx)

		tool := &mockTool{
			name: "Bash",
			checkFn: func(map[string]any, *Context) Decision {
				return Decision{Behavior: BehaviorAsk, BypassImmune: true}
			},
		}
		engine.AddRule(Rule{ToolName: "Bash", Behavior: BehaviorAsk, Source: "test"})

		d, _ := engine.CheckPermission(tool, nil)
		if d.Behavior == BehaviorAsk {
			t.Fatalf("mode %s: DontAsk must never return Ask", mode)
		}
	}
}

func TestRuleMatching_ContentMatching(t *testing.T) {
	tool := &mockTool{
		name: "Bash",
		matchFn: func(content string, input map[string]any) bool {
			cmd, _ := input["command"].(string)
			return len(cmd) >= len(content) && cmd[:len(content)] == content
		},
	}
	ctx := NewContext(ModeDefault)
	engine := NewEngine(ctx)
	engine.AddRule(Rule{ToolName: "Bash", RuleContent: "git", Behavior: BehaviorAllow, Source: "test"})

	d, _ := engine.CheckPermission(tool, map[string]any{"command": "git status"})
	if d.Behavior != BehaviorAllow {
		t.Fatalf("expected Allow for 'git status', got %s", d.Behavior)
	}

	d, _ = engine.CheckPermission(tool, map[string]any{"command": "rm -rf /"})
	if d.Behavior != BehaviorAsk {
		t.Fatalf("expected Ask for 'rm -rf /', got %s", d.Behavior)
	}
}

func TestRuleMatching_EmptyContentMatchesAll(t *testing.T) {
	ctx := NewContext(ModeDefault)
	engine := NewEngine(ctx)
	engine.AddRule(Rule{ToolName: "Bash", Behavior: BehaviorAllow, Source: "test"})

	d, _ := engine.CheckPermission(newTool("Bash"), map[string]any{"command": "anything"})
	if d.Behavior != BehaviorAllow {
		t.Fatalf("expected Allow, got %s", d.Behavior)
	}
}

func TestRuleMatching_DifferentToolNotAffected(t *testing.T) {
	ctx := NewContext(ModeDefault)
	engine := NewEngine(ctx)
	engine.AddRule(Rule{ToolName: "Bash", Behavior: BehaviorAllow, Source: "test"})

	d, _ := engine.CheckPermission(newTool("Write"), nil)
	if d.Behavior != BehaviorAsk {
		t.Fatalf("expected Ask (rule for different tool), got %s", d.Behavior)
	}
}

func TestAddRule_CategorizesCorrectly(t *testing.T) {
	ctx := NewContext(ModeDefault)
	engine := NewEngine(ctx)

	engine.AddRule(Rule{ToolName: "A", Behavior: BehaviorAllow, Source: "test"})
	engine.AddRule(Rule{ToolName: "B", Behavior: BehaviorDeny, Source: "test"})
	engine.AddRule(Rule{ToolName: "C", Behavior: BehaviorAsk, Source: "test"})

	if len(ctx.AllowRules["A"]) != 1 {
		t.Fatal("allow rule not added")
	}
	if len(ctx.DenyRules["B"]) != 1 {
		t.Fatal("deny rule not added")
	}
	if len(ctx.AskRules["C"]) != 1 {
		t.Fatal("ask rule not added")
	}
}

func TestSuggestionsInDecision(t *testing.T) {
	customSuggestions := []Rule{
		{ToolName: "Bash", RuleContent: "git:*", Behavior: BehaviorAllow, Source: "suggested"},
		{ToolName: "Bash", RuleContent: "npm:*", Behavior: BehaviorAllow, Source: "suggested"},
	}
	tool := &mockTool{
		name: "Bash",
		suggestFn: func(map[string]any) []Rule {
			return customSuggestions
		},
	}
	ctx := NewContext(ModeDefault)
	engine := NewEngine(ctx)

	d, _ := engine.CheckPermission(tool, nil)
	if len(d.SuggestedRules) != 2 {
		t.Fatalf("expected 2 suggestions, got %d", len(d.SuggestedRules))
	}
}

func TestUnknownMode_ReturnsError(t *testing.T) {
	ctx := NewContext("invalid_mode")
	engine := NewEngine(ctx)

	_, err := engine.CheckPermission(newTool("Bash"), nil)
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
}
