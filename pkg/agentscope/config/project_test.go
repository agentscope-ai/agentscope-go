package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadProjectConfig_Empty(t *testing.T) {
	dir := t.TempDir()

	cfg, err := LoadProjectConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.Instructions != "" {
		t.Errorf("expected empty instructions, got %q", cfg.Instructions)
	}
	if len(cfg.PermissionRules) != 0 {
		t.Errorf("expected no rules, got %d", len(cfg.PermissionRules))
	}
}

func TestLoadProjectConfig_Instructions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), "# Project rules\nDo the thing.")

	cfg, err := LoadProjectConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Instructions != "# Project rules\nDo the thing." {
		t.Errorf("instructions not loaded, got %q", cfg.Instructions)
	}
}

func TestLoadProjectConfig_InstructionPriority(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), "claude")
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "agents")
	writeFile(t, filepath.Join(dir, "README.md"), "readme")

	cfg, err := LoadProjectConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Instructions != "claude" {
		t.Errorf("expected CLAUDE.md to win, got %q", cfg.Instructions)
	}
}

func TestLoadProjectConfig_Settings(t *testing.T) {
	dir := t.TempDir()
	settings := `{
		"permissions": {
			"allow": [{"tool": "Bash", "content": "ls"}],
			"deny": [{"tool": "Write", "content": "/etc/*"}]
		},
		"tools": {"allowed": ["Bash", "Read"], "denied": ["Write"]},
		"model": "claude-sonnet-4-6",
		"hooks": {"pre_reply": [{"command": "echo hi", "timeout": 10}]},
		"custom": {"foo": "bar"}
	}`
	writeFile(t, filepath.Join(dir, ".agentscope", "settings.json"), settings)

	cfg, err := LoadProjectConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cfg.PermissionRules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(cfg.PermissionRules))
	}
	allow := cfg.PermissionRules[0]
	if allow.ToolName != "Bash" || allow.RuleContent != "ls" || allow.Behavior != permission.BehaviorAllow || allow.Source != "project" {
		t.Errorf("unexpected allow rule: %+v", allow)
	}
	deny := cfg.PermissionRules[1]
	if deny.ToolName != "Write" || deny.Behavior != permission.BehaviorDeny {
		t.Errorf("unexpected deny rule: %+v", deny)
	}

	if len(cfg.AllowedTools) != 2 || cfg.DeniedTools[0] != "Write" {
		t.Errorf("tools not loaded: allowed=%v denied=%v", cfg.AllowedTools, cfg.DeniedTools)
	}
	if cfg.ModelPreference != "claude-sonnet-4-6" {
		t.Errorf("model not loaded, got %q", cfg.ModelPreference)
	}
	if h := cfg.Hooks["pre_reply"]; len(h) != 1 || h[0].Command != "echo hi" || h[0].Timeout != 10 {
		t.Errorf("hooks not loaded: %+v", cfg.Hooks)
	}
	if cfg.CustomSettings["foo"] != "bar" {
		t.Errorf("custom not loaded: %+v", cfg.CustomSettings)
	}
	if cfg.ConfigDir != filepath.Join(dir, ".agentscope") {
		t.Errorf("config dir not set, got %q", cfg.ConfigDir)
	}
}

func TestLoadProjectConfig_LocalOverride(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".agentscope", "settings.json"), `{
		"model": "base-model",
		"permissions": {"allow": [{"tool": "Bash", "content": "ls"}]},
		"custom": {"a": 1, "b": 2}
	}`)
	writeFile(t, filepath.Join(dir, ".agentscope", "settings.local.json"), `{
		"model": "local-model",
		"permissions": {"deny": [{"tool": "Write"}]},
		"custom": {"b": 3, "c": 4}
	}`)

	cfg, err := LoadProjectConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.ModelPreference != "local-model" {
		t.Errorf("local model should override, got %q", cfg.ModelPreference)
	}
	if len(cfg.PermissionRules) != 2 {
		t.Errorf("expected merged allow+deny rules, got %d", len(cfg.PermissionRules))
	}
	if cfg.CustomSettings["a"] != float64(1) || cfg.CustomSettings["b"] != float64(3) || cfg.CustomSettings["c"] != float64(4) {
		t.Errorf("custom merge wrong: %+v", cfg.CustomSettings)
	}
}

func TestLoadProjectConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".agentscope", "settings.json"), `{not valid json`)

	_, err := LoadProjectConfig(dir)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestFindProjectRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "x")
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got := FindProjectRoot(nested)
	// t.TempDir may live under a symlinked path (/var -> /private/var on macOS);
	// compare resolved paths.
	gotResolved, _ := filepath.EvalSymlinks(got)
	rootResolved, _ := filepath.EvalSymlinks(root)
	if gotResolved != rootResolved {
		t.Errorf("expected root %q, got %q", rootResolved, gotResolved)
	}
}

func TestFindProjectRoot_AgentscopeMarker(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".agentscope"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	nested := filepath.Join(root, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got, _ := filepath.EvalSymlinks(FindProjectRoot(nested))
	want, _ := filepath.EvalSymlinks(root)
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}
