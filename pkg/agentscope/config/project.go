package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

// ProjectConfig holds all project-level configuration loaded from disk.
type ProjectConfig struct {
	// Instructions from CLAUDE.md / AGENTS.md (raw markdown)
	Instructions string `json:"instructions,omitempty"`

	// PermissionRules loaded from .agentscope/settings.json
	PermissionRules []permission.Rule `json:"permission_rules,omitempty"`

	// AllowedTools is a whitelist of tool names. Empty = all tools allowed.
	AllowedTools []string `json:"allowed_tools,omitempty"`

	// DeniedTools is a blacklist of tool names.
	DeniedTools []string `json:"denied_tools,omitempty"`

	// ModelPreference is the preferred model name (e.g. "claude-sonnet-4-6")
	ModelPreference string `json:"model_preference,omitempty"`

	// Hooks maps event names to shell commands to execute.
	Hooks map[string][]HookConfig `json:"hooks,omitempty"`

	// CustomSettings holds arbitrary user-defined settings.
	CustomSettings map[string]any `json:"custom_settings,omitempty"`

	// ConfigDir is the resolved path to .agentscope/ directory.
	ConfigDir string `json:"-"`
}

// HookConfig defines a shell command to run on an event.
type HookConfig struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // seconds, default 30
}

// instructionFiles are the candidate filenames for project instructions,
// checked in priority order within each directory.
var instructionFiles = []string{"CLAUDE.md", "AGENTS.md", "README.md"}

// maxInstructionBytes caps the concatenated instruction payload (codex's
// project_doc_max_bytes precedent is 32 KiB): oversized docs would crowd out
// the working context. The tail is cut on a rune boundary with a marker.
const maxInstructionBytes = 32 * 1024

// LoadProjectConfig loads configuration from the given project directory.
// It looks for:
//   - instruction files (CLAUDE.md / AGENTS.md / README.md) layered from the
//     project root down to projectDir — codex AGENTS.md parity: each
//     directory contributes its first matching file, concatenated root→cwd
//     so deeper, more specific instructions land last
//   - .agentscope/settings.json for structured settings
//   - .agentscope/settings.local.json for local overrides (merged on top)
//
// Returns a zero-value ProjectConfig (not error) if no config files exist.
func LoadProjectConfig(projectDir string) (*ProjectConfig, error) {
	cfg := &ProjectConfig{}

	root := FindProjectRoot(projectDir)
	if root == "" {
		root = projectDir
	}

	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		absDir = projectDir
	}
	// Instructions layer from the VCS root: an instruction file must be
	// content, not a boundary — a nested CLAUDE.md should stack on the root
	// one, not replace it. Settings keep using the generic FindProjectRoot
	// anchor above.
	instructions, err := loadInstructions(findInstructionRoot(absDir), absDir)
	if err != nil {
		return nil, err
	}
	cfg.Instructions = instructions

	configDir := filepath.Join(root, ".agentscope")
	if fi, err := os.Stat(configDir); err == nil && fi.IsDir() {
		cfg.ConfigDir = configDir
	}

	base, err := loadSettings(filepath.Join(root, ".agentscope", "settings.json"))
	if err != nil {
		return nil, err
	}
	local, err := loadSettings(filepath.Join(root, ".agentscope", "settings.local.json"))
	if err != nil {
		return nil, err
	}

	settings := mergeSettings(base, local)
	if settings == nil {
		return cfg, nil
	}

	if settings.Permissions != nil {
		for _, r := range settings.Permissions.Allow {
			cfg.PermissionRules = append(cfg.PermissionRules, permission.Rule{
				ToolName:    r.Tool,
				RuleContent: r.Content,
				Behavior:    permission.BehaviorAllow,
				Source:      "project",
			})
		}
		for _, r := range settings.Permissions.Deny {
			cfg.PermissionRules = append(cfg.PermissionRules, permission.Rule{
				ToolName:    r.Tool,
				RuleContent: r.Content,
				Behavior:    permission.BehaviorDeny,
				Source:      "project",
			})
		}
	}

	if settings.Tools != nil {
		cfg.AllowedTools = settings.Tools.Allowed
		cfg.DeniedTools = settings.Tools.Denied
	}

	cfg.ModelPreference = settings.Model
	cfg.Hooks = settings.Hooks
	cfg.CustomSettings = settings.Custom

	return cfg, nil
}

// FindProjectRoot walks up from startDir looking for a directory containing
// .agentscope/, .git/, or CLAUDE.md. Returns "" if none found.
func FindProjectRoot(startDir string) string {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		dir = startDir
	}

	for {
		if hasProjectMarker(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func hasProjectMarker(dir string) bool {
	for _, marker := range []string{".agentscope", ".git", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}

// findInstructionRoot anchors the instruction layering at the nearest
// ancestor with a .git entry (codex's default project-root marker). Without
// one it falls back to FindProjectRoot (.agentscope / instruction-file
// markers), preserving the pre-layering single-file behavior for marker-less
// trees.
func findInstructionRoot(dir string) string {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if root := FindProjectRoot(dir); root != "" {
		return root
	}
	return dir
}

// loadInstructions collects instruction files from root down to dir
// (inclusive), concatenating in root→cwd order with a blank line between
// layers. A directory contributes its first matching file only. When dir is
// not under root (or equals it), only the root layer applies — the
// historical single-file behavior. The total payload is capped at
// maxInstructionBytes.
func loadInstructions(root, dir string) (string, error) {
	chain := []string{root}
	if rel, err := filepath.Rel(root, dir); err == nil && rel != "." && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		cur := root
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			chain = append(chain, cur)
		}
	}

	var sb strings.Builder
	for _, dir := range chain {
		for _, name := range instructionFiles {
			p := filepath.Join(dir, name)
			data, err := os.ReadFile(p)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return "", fmt.Errorf("config: read %s: %w", p, err)
			}
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.Write(data)
			// One file per directory: deeper fallbacks (e.g. AGENTS.md when
			// CLAUDE.md exists) do not stack within the same directory.
			break
		}
	}

	out := sb.String()
	if len(out) > maxInstructionBytes {
		cut := maxInstructionBytes
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + "\n\n[... instructions truncated at 32 KiB ...]"
	}
	return out, nil
}
