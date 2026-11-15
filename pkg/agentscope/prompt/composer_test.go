package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/skill"
)

func TestComposeOrder(t *testing.T) {
	c := NewPromptComposer()
	c.Add(PromptSection{Name: "c", Priority: 30, Content: "third"})
	c.Add(PromptSection{Name: "a", Priority: 10, Content: "first"})
	c.Add(PromptSection{Name: "b", Priority: 20, Content: "second"})

	got := c.Compose()
	want := "first\n\nsecond\n\nthird"
	if got != want {
		t.Errorf("Compose() = %q, want %q", got, want)
	}
}

func TestComposeStableByName(t *testing.T) {
	c := NewPromptComposer()
	c.Add(PromptSection{Name: "z", Priority: 10, Content: "z"})
	c.Add(PromptSection{Name: "a", Priority: 10, Content: "a"})

	got := c.Compose()
	if got != "a\n\nz" {
		t.Errorf("equal-priority sections should sort by name, got %q", got)
	}
}

func TestSetSectionReplaces(t *testing.T) {
	c := NewPromptComposer()
	c.Add(PromptSection{Name: "base", Priority: 5, Content: "original"})
	c.SetSection("base", "replaced")

	secs := c.Sections()
	if len(secs) != 1 {
		t.Fatalf("expected 1 section, got %d", len(secs))
	}
	if secs[0].Content != "replaced" {
		t.Errorf("content = %q, want replaced", secs[0].Content)
	}
	if secs[0].Priority != 5 {
		t.Errorf("SetSection should preserve priority, got %d", secs[0].Priority)
	}
}

func TestSetSectionAddsNew(t *testing.T) {
	c := NewPromptComposer()
	c.SetSection("new", "content")
	if c.Compose() != "content" {
		t.Errorf("SetSection should add new section, got %q", c.Compose())
	}
}

func TestRemoveSection(t *testing.T) {
	c := NewPromptComposer()
	c.Add(PromptSection{Name: "a", Priority: 1, Content: "a"})
	c.Add(PromptSection{Name: "b", Priority: 2, Content: "b"})
	c.RemoveSection("a")

	got := c.Compose()
	if got != "b" {
		t.Errorf("after removing a, Compose() = %q, want b", got)
	}
	c.RemoveSection("nonexistent") // should not panic
}

func TestEmptyComposer(t *testing.T) {
	c := NewPromptComposer()
	if got := c.Compose(); got != "" {
		t.Errorf("empty composer Compose() = %q, want empty", got)
	}
	if len(c.Sections()) != 0 {
		t.Error("empty composer should have no sections")
	}
}

func TestComposeSkipsEmptyContent(t *testing.T) {
	c := NewPromptComposer()
	c.Add(PromptSection{Name: "a", Priority: 1, Content: "a"})
	c.Add(PromptSection{Name: "empty", Priority: 2, Content: "   "})
	c.Add(PromptSection{Name: "b", Priority: 3, Content: "b"})

	if got := c.Compose(); got != "a\n\nb" {
		t.Errorf("Compose() = %q, want %q", got, "a\n\nb")
	}
}

func TestEnvironmentProvider(t *testing.T) {
	s := EnvironmentProvider()
	if s.Name != SectionEnvironment {
		t.Errorf("name = %q, want %q", s.Name, SectionEnvironment)
	}
	if strings.TrimSpace(s.Content) == "" {
		t.Error("EnvironmentProvider should return non-empty content")
	}
	if !strings.Contains(s.Content, "OS:") {
		t.Error("environment content should mention OS")
	}
}

func TestProjectConfigProviderNonExistentDir(t *testing.T) {
	s, err := ProjectConfigProvider("/nonexistent/dir/xyz")
	if err != nil {
		t.Errorf("non-existent dir should not error, got %v", err)
	}
	if strings.TrimSpace(s.Content) != "" {
		t.Errorf("non-existent dir should yield empty content, got %q", s.Content)
	}
}

func TestProjectConfigProviderReadsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("project rules here"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := ProjectConfigProvider(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Content, "project rules here") {
		t.Errorf("content should include file body, got %q", s.Content)
	}
}

func TestSkillListProvider(t *testing.T) {
	skills := []skill.Skill{{Name: "coding", Description: "Write code"}}
	s := SkillListProvider(skills)
	if s.Name != SectionSkills {
		t.Errorf("name = %q, want %q", s.Name, SectionSkills)
	}
	if !strings.Contains(s.Content, "coding") {
		t.Errorf("skill section should list skill name, got %q", s.Content)
	}
}

func TestGitStatusProviderNonGitDir(t *testing.T) {
	dir := t.TempDir()
	s, err := GitStatusProvider(dir)
	if err != nil {
		t.Errorf("non-git dir should not error, got %v", err)
	}
	if strings.TrimSpace(s.Content) != "" {
		t.Errorf("non-git dir should yield empty content, got %q", s.Content)
	}
}
