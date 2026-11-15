package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

func TestSettingsRuleStore_AppendCreatesFile(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSettingsRuleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	rule := permission.Rule{ToolName: "Bash", RuleContent: "ls:*", Behavior: permission.BehaviorAllow, Source: "user"}
	if err := store.AppendRule(rule); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	perms, ok := doc["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions section missing: %s", data)
	}
	allow, ok := perms["allow"].([]any)
	if !ok || len(allow) != 1 {
		t.Fatalf("allow list wrong: %s", data)
	}
	entry, _ := allow[0].(map[string]any)
	if entry["tool"] != "Bash" || entry["content"] != "ls:*" {
		t.Errorf("wrong rule entry: %v", entry)
	}
}

func TestSettingsRuleStore_PreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	settingsDir := filepath.Join(dir, ".agentscope")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"model": "gpt-x", "permissions": {"allow": [{"tool": "Read", "content": ""}], "deny": [{"tool": "Bash", "content": "rm:*"}]}}`
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.local.json"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := NewSettingsRuleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	rule := permission.Rule{ToolName: "Bash", RuleContent: "git status:*", Behavior: permission.BehaviorAllow, Source: "user"}
	if err := store.AppendRule(rule); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "gpt-x" {
		t.Errorf("unrelated field lost: %s", data)
	}
	perms := doc["permissions"].(map[string]any)
	if got := len(perms["allow"].([]any)); got != 2 {
		t.Errorf("existing allow rule lost, want 2 got %d", got)
	}
	if got := len(perms["deny"].([]any)); got != 1 {
		t.Errorf("existing deny rule lost, want 1 got %d", got)
	}
}

func TestSettingsRuleStore_RejectsAskRules(t *testing.T) {
	store, err := NewSettingsRuleStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rule := permission.Rule{ToolName: "Bash", RuleContent: "make:*", Behavior: permission.BehaviorAsk, Source: "user"}
	if err := store.AppendRule(rule); err == nil {
		t.Error("ask rules cannot be represented in settings and must be rejected")
	}
}

// TestSettingsRuleStore_LoadedBackByProjectConfig closes the loop: a learned
// rule must apply to future sessions through the regular load path.
func TestSettingsRuleStore_LoadedBackByProjectConfig(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSettingsRuleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	allow := permission.Rule{ToolName: "Bash", RuleContent: "ls:*", Behavior: permission.BehaviorAllow, Source: "user"}
	deny := permission.Rule{ToolName: "Bash", RuleContent: "rm:*", Behavior: permission.BehaviorDeny, Source: "user"}
	if err := store.AppendRule(allow); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRule(deny); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadProjectConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	var sawAllow, sawDeny bool
	for _, r := range cfg.PermissionRules {
		switch {
		case r.Behavior == permission.BehaviorAllow && r.RuleContent == "ls:*":
			sawAllow = true
		case r.Behavior == permission.BehaviorDeny && r.RuleContent == "rm:*":
			sawDeny = true
		}
	}
	if !sawAllow || !sawDeny {
		t.Errorf("persisted rules not loaded back (allow=%v deny=%v): %+v", sawAllow, sawDeny, cfg.PermissionRules)
	}
}
