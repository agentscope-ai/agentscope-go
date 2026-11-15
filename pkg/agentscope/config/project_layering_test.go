package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Codex AGENTS.md parity: instruction files layer from the project root down
// to the working directory, root first.
func TestLoadProjectConfig_LayeredInstructions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root rules")
	sub := filepath.Join(root, "pkg", "feature")
	writeFile(t, filepath.Join(root, "pkg", "AGENTS.md"), "pkg rules")
	writeFile(t, filepath.Join(sub, "AGENTS.md"), "feature rules")

	cfg, err := LoadProjectConfig(sub)
	if err != nil {
		t.Fatal(err)
	}
	// All three layers, root first, deepest last.
	ri := strings.Index(cfg.Instructions, "root rules")
	pi := strings.Index(cfg.Instructions, "pkg rules")
	fi := strings.Index(cfg.Instructions, "feature rules")
	if ri < 0 || pi < 0 || fi < 0 {
		t.Fatalf("missing layers: %q", cfg.Instructions)
	}
	if !(ri < pi && pi < fi) {
		t.Errorf("layers out of order: %q", cfg.Instructions)
	}
}

// A nested CLAUDE.md must not reset the layering root (instruction files are
// content, not boundaries).
func TestLoadProjectConfig_NestedInstructionFileDoesNotResetRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref")
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "root claude")
	sub := filepath.Join(root, "sub")
	writeFile(t, filepath.Join(sub, "CLAUDE.md"), "sub claude")

	cfg, err := LoadProjectConfig(sub)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg.Instructions, "root claude") || !strings.Contains(cfg.Instructions, "sub claude") {
		t.Errorf("expected both layers, got %q", cfg.Instructions)
	}
}

// Within one directory, the first matching filename wins; deeper fallbacks
// do not stack.
func TestLoadProjectConfig_OneFilePerDirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref")
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "claude wins")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "agents loses")

	cfg, err := LoadProjectConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg.Instructions, "claude wins") {
		t.Errorf("priority file missing: %q", cfg.Instructions)
	}
	if strings.Contains(cfg.Instructions, "agents loses") {
		t.Errorf("fallback file should not stack: %q", cfg.Instructions)
	}
}

// No .git anywhere: the generic project-root anchor keeps the historical
// single-file behavior.
func TestLoadProjectConfig_NoGitFallsBackToProjectRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".agentscope", "settings.json"), "{}")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root only")
	sub := filepath.Join(root, "deep", "deeper")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadProjectConfig(sub)
	if err != nil {
		t.Fatal(err)
	}
	// .agentscope anchors the project at root; the chain root->cwd has no
	// other instruction files, so only the root layer applies.
	if cfg.Instructions != "root only" {
		t.Errorf("expected single root layer, got %q", cfg.Instructions)
	}
}

// The concatenated payload is capped to keep oversized docs from crowding
// out the working context.
func TestLoadProjectConfig_InstructionBudget(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref")
	writeFile(t, filepath.Join(root, "AGENTS.md"), strings.Repeat("a", maxInstructionBytes))
	sub := filepath.Join(root, "sub")
	writeFile(t, filepath.Join(sub, "AGENTS.md"), "tail that overflows")

	cfg, err := LoadProjectConfig(sub)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg.Instructions, "instructions truncated") {
		t.Error("expected truncation marker")
	}
	if strings.Contains(cfg.Instructions, "tail that overflows") {
		t.Error("overflow layer should have been cut")
	}
	if len(cfg.Instructions) > maxInstructionBytes+128 {
		t.Errorf("payload exceeds budget: %d bytes", len(cfg.Instructions))
	}
}
