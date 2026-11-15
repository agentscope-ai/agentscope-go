package permission

import (
	"fmt"
	"testing"
)

type failingStore struct{}

func (failingStore) AppendRule(Rule) error { return fmt.Errorf("disk full") }

type recordingStore struct{ rules []Rule }

func (s *recordingStore) AppendRule(r Rule) error {
	s.rules = append(s.rules, r)
	return nil
}

func TestAddRulePersistent_NoStore(t *testing.T) {
	e := NewEngine(NewContext(ModeDefault))
	rule := Rule{ToolName: "Bash", RuleContent: "ls:*", Behavior: BehaviorAllow, Source: "user"}
	if err := e.AddRulePersistent(rule); err != nil {
		t.Fatalf("no-store persistence must degrade to in-memory, got %v", err)
	}
	if got := e.Context.AllowRules["Bash"]; len(got) != 1 {
		t.Errorf("rule not applied in memory: %v", got)
	}
}

func TestAddRulePersistent_StoreFailureNotApplied(t *testing.T) {
	e := NewEngine(NewContext(ModeDefault))
	e.SetRuleStore(failingStore{})
	rule := Rule{ToolName: "Bash", RuleContent: "rm -rf /", Behavior: BehaviorAllow, Source: "user"}
	if err := e.AddRulePersistent(rule); err == nil {
		t.Fatal("expected store failure to surface")
	}
	// A rule the user believes is persisted must not silently apply in
	// memory only — next session it would vanish, and the mismatch between
	// believed and actual policy is the dangerous part.
	if got := e.Context.AllowRules["Bash"]; len(got) != 0 {
		t.Errorf("failed-persist rule must not be applied, got %v", got)
	}
}

func TestAddRulePersistent_WritesThrough(t *testing.T) {
	e := NewEngine(NewContext(ModeDefault))
	store := &recordingStore{}
	e.SetRuleStore(store)
	rule := Rule{ToolName: "Bash", RuleContent: "git status:*", Behavior: BehaviorAllow, Source: "user"}
	if err := e.AddRulePersistent(rule); err != nil {
		t.Fatal(err)
	}
	if len(store.rules) != 1 || store.rules[0].RuleContent != "git status:*" {
		t.Errorf("store did not receive the rule: %v", store.rules)
	}
	if got := e.Context.AllowRules["Bash"]; len(got) != 1 {
		t.Errorf("rule not applied in memory: %v", got)
	}
}
