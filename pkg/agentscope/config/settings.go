package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// settingsJSON is the on-disk format of .agentscope/settings.json
type settingsJSON struct {
	Permissions *permissionsConfig      `json:"permissions,omitempty"`
	Tools       *toolsConfig            `json:"tools,omitempty"`
	Model       string                  `json:"model,omitempty"`
	Hooks       map[string][]HookConfig `json:"hooks,omitempty"`
	Custom      map[string]any          `json:"custom,omitempty"`
}

type permissionsConfig struct {
	Allow []ruleJSON `json:"allow,omitempty"`
	Deny  []ruleJSON `json:"deny,omitempty"`
}

type ruleJSON struct {
	Tool    string `json:"tool"`
	Content string `json:"content,omitempty"`
}

type toolsConfig struct {
	Allowed []string `json:"allowed,omitempty"`
	Denied  []string `json:"denied,omitempty"`
}

// loadSettings reads and parses .agentscope/settings.json.
// Returns nil, nil if file doesn't exist.
func loadSettings(path string) (*settingsJSON, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var s settingsJSON
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return &s, nil
}

// mergeSettings merges local settings on top of base settings.
func mergeSettings(base, local *settingsJSON) *settingsJSON {
	if base == nil {
		return local
	}
	if local == nil {
		return base
	}

	merged := *base

	if local.Permissions != nil {
		if merged.Permissions == nil {
			merged.Permissions = local.Permissions
		} else {
			p := *merged.Permissions
			p.Allow = append(p.Allow, local.Permissions.Allow...)
			p.Deny = append(p.Deny, local.Permissions.Deny...)
			merged.Permissions = &p
		}
	}

	if local.Tools != nil {
		merged.Tools = local.Tools
	}

	if local.Model != "" {
		merged.Model = local.Model
	}

	if len(local.Hooks) > 0 {
		if merged.Hooks == nil {
			merged.Hooks = make(map[string][]HookConfig, len(local.Hooks))
		} else {
			h := make(map[string][]HookConfig, len(merged.Hooks)+len(local.Hooks))
			for k, v := range merged.Hooks {
				h[k] = v
			}
			merged.Hooks = h
		}
		for k, v := range local.Hooks {
			merged.Hooks[k] = v
		}
	}

	if len(local.Custom) > 0 {
		if merged.Custom == nil {
			merged.Custom = make(map[string]any, len(local.Custom))
		} else {
			c := make(map[string]any, len(merged.Custom)+len(local.Custom))
			for k, v := range merged.Custom {
				c[k] = v
			}
			merged.Custom = c
		}
		for k, v := range local.Custom {
			merged.Custom[k] = v
		}
	}

	return &merged
}
