package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgenticMemory_LayoutAndEmptySnapshot(t *testing.T) {
	workdir := t.TempDir()
	m, err := NewAgenticMemory(AgenticMemoryConfig{Workdir: workdir})
	if err != nil {
		t.Fatal(err)
	}

	prompt := m.OnSystemPrompt(context.Background(), "agent", "You are helpful.")

	// Layout created.
	if _, err := os.Stat(filepath.Join(workdir, DefaultAgenticMemoryDir, FilenameMemoryMD)); err != nil {
		t.Errorf("layout must be ensured: %v", err)
	}
	// Instructions injected with the memory dir substituted.
	if !strings.Contains(prompt, filepath.Join(workdir, DefaultAgenticMemoryDir)) {
		t.Error("memory dir must be substituted into the instructions")
	}
	if strings.Contains(prompt, "{memory_dir}") {
		t.Error("placeholder must not survive")
	}
	// Empty snapshot placeholder.
	if !strings.Contains(prompt, "Your MEMORY.md is currently empty") {
		t.Error("empty placeholder missing")
	}
	if !strings.HasPrefix(prompt, "You are helpful.") {
		t.Error("existing prompt must be preserved")
	}
}

func TestAgenticMemory_InjectsMemoryContent(t *testing.T) {
	workdir := t.TempDir()
	m, err := NewAgenticMemory(AgenticMemoryConfig{Workdir: workdir, MemoryDir: "mems"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ensureLayout(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.MemoryMDPath(), []byte("- user prefers terse answers\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prompt := m.OnSystemPrompt(context.Background(), "agent", "base")
	if !strings.Contains(prompt, "## MEMORY.md") || !strings.Contains(prompt, "user prefers terse answers") {
		t.Errorf("snapshot missing: %s", prompt)
	}
	if !strings.Contains(prompt, filepath.Join(workdir, "mems")) {
		t.Error("custom memory dir must be used")
	}
}

func TestAgenticMemory_TruncatesSnapshot(t *testing.T) {
	workdir := t.TempDir()
	m, err := NewAgenticMemory(AgenticMemoryConfig{Workdir: workdir, MaxTokens: 50})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ensureLayout(); err != nil {
		t.Fatal(err)
	}
	// ~200 tokens of content (800 bytes).
	lines := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		lines = append(lines, "- memory line "+strings.Repeat("z", 16))
	}
	if err := os.WriteFile(m.MemoryMDPath(), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	prompt := m.OnSystemPrompt(context.Background(), "agent", "base")
	if !strings.Contains(prompt, "<<<TRUNCATED>>>") {
		t.Fatal("truncation marker missing")
	}
	if !strings.Contains(prompt, "system-reminder") || !strings.Contains(prompt, "Read") {
		t.Error("truncation must point at the Read tool")
	}
	// The injected snapshot itself respects the budget.
	idx := strings.Index(prompt, "## MEMORY.md")
	if idx < 0 {
		t.Fatal("MEMORY.md section missing")
	}
	snap := prompt[idx+len("## MEMORY.md"):]
	if end := strings.Index(snap, "<<<TRUNCATED>>>"); end < 0 {
		t.Fatal("truncation marker missing in snapshot")
	} else {
		snap = snap[:end]
	}
	if estimateTokens(snap) > 50 {
		t.Errorf("snapshot exceeds budget: %d tokens", estimateTokens(snap))
	}
}

func TestTruncateToTokens(t *testing.T) {
	if got := truncateToTokens("short", 100); got != "short" {
		t.Errorf("under-budget content must pass through, got %q", got)
	}
	if got := truncateToTokens("anything", 0); got != "" {
		t.Errorf("zero budget must empty, got %q", got)
	}
	long := strings.Repeat("abcd", 100) // 400 bytes = 100 tokens
	got := truncateToTokens(long, 25)
	if estimateTokens(got) > 25 {
		t.Errorf("truncated result over budget: %d", estimateTokens(got))
	}
	// Multi-byte safety: never split a rune.
	uni := strings.Repeat("好", 200) // 600 bytes = 150 tokens
	got = truncateToTokens(uni, 10)
	if estimateTokens(got) > 10 {
		t.Errorf("unicode truncation over budget: %d", estimateTokens(got))
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatal("truncation split a rune")
		}
	}
}

func TestNewAgenticMemory_RequiresWorkdir(t *testing.T) {
	if _, err := NewAgenticMemory(AgenticMemoryConfig{}); err == nil {
		t.Error("missing workdir must error")
	}
}
