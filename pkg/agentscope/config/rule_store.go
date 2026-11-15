package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

// SettingsRuleStore is a permission.RuleStore writing learned rules into
// .agentscope/settings.local.json — the local-override file, so learned
// rules are not committed with shared project settings. Other settings
// fields are preserved byte-for-key through a generic map round-trip.
type SettingsRuleStore struct {
	mu   sync.Mutex
	path string
}

// NewSettingsRuleStore resolves the project root from dir (see
// FindProjectRoot) and targets its .agentscope/settings.local.json.
// Appends are mutex-guarded within one instance; hosts must share a single
// instance per project root — two stores over the same file can lose rules
// to read-modify-write races.
func NewSettingsRuleStore(dir string) (*SettingsRuleStore, error) {
	root := FindProjectRoot(dir)
	if root == "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("config: resolve %s: %w", dir, err)
		}
		root = abs
	}
	return &SettingsRuleStore{path: filepath.Join(root, ".agentscope", "settings.local.json")}, nil
}

// Path returns the backing file path.
func (s *SettingsRuleStore) Path() string { return s.path }

// AppendRule implements permission.RuleStore. Allow and Deny rules map to the
// permissions.allow/permissions.denied lists; Ask rules cannot be represented
// in the settings format and are rejected.
func (s *SettingsRuleStore) AppendRule(rule permission.Rule) error {
	var section string
	switch rule.Behavior {
	case permission.BehaviorAllow:
		section = "allow"
	case permission.BehaviorDeny:
		section = "deny"
	default:
		return fmt.Errorf("config: cannot persist %s rules to settings", rule.Behavior)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	doc := map[string]any{}
	data, err := os.ReadFile(s.path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("config: read %s: %w", s.path, err)
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("config: parse %s: %w", s.path, err)
		}
	}

	perms, _ := doc["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
		doc["permissions"] = perms
	}
	list, _ := perms[section].([]any)
	list = append(list, map[string]any{"tool": rule.ToolName, "content": rule.RuleContent})
	perms[section] = list

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encode settings: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("config: create settings dir: %w", err)
	}
	return os.WriteFile(s.path, append(out, '\n'), 0o644)
}
