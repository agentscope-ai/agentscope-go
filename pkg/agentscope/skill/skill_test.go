package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkillMD(t *testing.T, dir, name, desc, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// --- parseSKILLMD ---

func TestParseSKILLMD(t *testing.T) {
	data := []byte("---\nname: test\ndescription: A test skill\n---\n\nDo things step by step.")
	s, err := parseSKILLMD(data, "/some/dir", 1000.0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "test" {
		t.Errorf("expected name=test, got %s", s.Name)
	}
	if s.Description != "A test skill" {
		t.Errorf("expected description, got %s", s.Description)
	}
	if s.Markdown != "Do things step by step." {
		t.Errorf("unexpected body: %q", s.Markdown)
	}
	if s.Dir != "/some/dir" {
		t.Errorf("unexpected dir: %s", s.Dir)
	}
}

func TestParseSKILLMD_DelimiterLines(t *testing.T) {
	const body = "Use foo---bar.\n\n---\n\nKeep the horizontal rule."
	tests := []struct {
		name         string
		frontmatter  string
		wantName     string
		wantDesc     string
		wantCategory string
	}{
		{
			name:        "plain description",
			frontmatter: "name: test\ndescription: Convert foo---bar safely",
			wantName:    "test", wantDesc: "Convert foo---bar safely",
		},
		{
			name:        "quoted description",
			frontmatter: "name: test\ndescription: \"Convert foo---bar safely\"",
			wantName:    "test", wantDesc: "Convert foo---bar safely",
		},
		{
			name:        "plain name",
			frontmatter: "name: test---skill\ndescription: A test skill",
			wantName:    "test---skill", wantDesc: "A test skill",
		},
		{
			name:        "quoted name",
			frontmatter: "name: \"test---skill\"\ndescription: A test skill",
			wantName:    "test---skill", wantDesc: "A test skill",
		},
		{
			name:        "category",
			frontmatter: "name: test\ndescription: A test skill\ncategory: group---one",
			wantName:    "test", wantDesc: "A test skill", wantCategory: "group---one",
		},
		{
			name:        "literal description",
			frontmatter: "name: test\ndescription: |\n  first\n  ---\n  last",
			wantName:    "test", wantDesc: "first\n---\nlast\n",
		},
		{
			name:        "folded description",
			frontmatter: "name: test\ndescription: >-\n  first\n  ---\n  last",
			wantName:    "test", wantDesc: "first --- last",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte("---\n" + tt.frontmatter + "\n---\n\n" + body)
			s, err := parseSKILLMD(data, "/dir", 0)
			if err != nil {
				t.Fatal(err)
			}
			if s.Name != tt.wantName || s.Description != tt.wantDesc || s.Category != tt.wantCategory {
				t.Errorf("unexpected metadata: name=%q description=%q category=%q", s.Name, s.Description, s.Category)
			}
			if s.Markdown != body {
				t.Errorf("unexpected body: %q", s.Markdown)
			}
		})
	}
}

func TestParseSKILLMD_DelimiterCompatibility(t *testing.T) {
	tests := []struct {
		name     string
		data     string
		wantBody string
	}{
		{"CRLF", "---\r\nname: test\r\ndescription: A test skill\r\n---\r\n\r\nfirst\r\n---\r\nlast\r\n", "first\r\n---\r\nlast"},
		{"closing delimiter trailing whitespace", "---\nname: test\ndescription: A test skill\n--- \t\n\nbody", "body"},
		{"leading whitespace", "\n \t---\nname: test\ndescription: A test skill\n---\n\nbody\n\n", "body"},
		{"opening prefix", "---name: test\ndescription: A test skill\n---\n\nbody", "body"},
		{"closing delimiter at EOF", "---\nname: test\ndescription: A test skill\n---", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := parseSKILLMD([]byte(tt.data), "/dir", 0)
			if err != nil {
				t.Fatal(err)
			}
			if s.Name != "test" || s.Description != "A test skill" || s.Markdown != tt.wantBody {
				t.Errorf("unexpected parsed skill: %+v", s)
			}
		})
	}
}

func TestParseSKILLMD_InvalidClosingDelimiter(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"missing", "---\nname: test\ndescription: A test skill\n"},
		{"inline metadata", "---\nname: test\ndescription: Convert foo---bar safely\n"},
		{"indented", "---\nname: test\ndescription: A test skill\n  ---\nbody"},
		{"suffixed", "---\nname: test\ndescription: A test skill\n---body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseSKILLMD([]byte(tt.data), "/dir", 0)
			if err == nil || !strings.Contains(err.Error(), "invalid frontmatter") {
				t.Errorf("expected invalid frontmatter, got %v", err)
			}
		})
	}
}

func TestParseSKILLMD_InvalidYAML(t *testing.T) {
	_, err := parseSKILLMD([]byte("---\nname: test\ndescription: [unclosed\n---\nbody"), "/dir", 0)
	if err == nil || !strings.Contains(err.Error(), "parse frontmatter") {
		t.Errorf("expected YAML parsing error, got %v", err)
	}
}

func TestParseSKILLMD_MissingFrontmatter(t *testing.T) {
	data := []byte("No frontmatter here.")
	_, err := parseSKILLMD(data, "/dir", 0)
	if err == nil {
		t.Error("expected error for missing frontmatter")
	}
}

func TestParseSKILLMD_MissingName(t *testing.T) {
	data := []byte("---\ndescription: only desc\n---\n\nbody")
	_, err := parseSKILLMD(data, "/dir", 0)
	if err == nil {
		t.Error("expected error for missing name")
	}
}

func TestParseSKILLMD_MissingDescription(t *testing.T) {
	data := []byte("---\nname: only_name\n---\n\nbody")
	_, err := parseSKILLMD(data, "/dir", 0)
	if err == nil {
		t.Error("expected error for missing description")
	}
}

// --- LocalSkillLoader ---

func TestLocalSkillLoader_SingleDir(t *testing.T) {
	dir := t.TempDir()
	writeSkillMD(t, dir, "coding", "Write code", "Use TDD always.")

	loader := NewLocalSkillLoader(dir, false)
	skills, err := loader.LoadSkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if skills[0].Name != "coding" {
		t.Errorf("expected name=coding, got %s", skills[0].Name)
	}
}

func TestLocalSkillLoader_ScanSubdirs(t *testing.T) {
	root := t.TempDir()
	writeSkillMD(t, filepath.Join(root, "skill_a"), "alpha", "First skill", "Alpha body.")
	writeSkillMD(t, filepath.Join(root, "skill_b"), "beta", "Second skill", "Beta body.")

	loader := NewLocalSkillLoader(root, true)
	skills, err := loader.LoadSkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(skills))
	}
}

func TestLocalSkillLoader_NoSkills(t *testing.T) {
	dir := t.TempDir()
	loader := NewLocalSkillLoader(dir, false)
	skills, err := loader.LoadSkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 0 {
		t.Errorf("expected 0 skills, got %d", len(skills))
	}
}

func TestLocalSkillLoader_Caching(t *testing.T) {
	dir := t.TempDir()
	writeSkillMD(t, dir, "cached", "Cached skill", "Original body.")

	loader := NewLocalSkillLoader(dir, false)

	// First load
	skills1, _ := loader.LoadSkills()
	if len(skills1) != 1 {
		t.Fatal("first load failed")
	}

	// Second load should use cache
	skills2, _ := loader.LoadSkills()
	if len(skills2) != 1 || skills2[0].Name != "cached" {
		t.Error("cached load should return same skill")
	}
}

// --- FormatSkillInstructions ---

func TestFormatSkillInstructions(t *testing.T) {
	skills := []Skill{
		{Name: "coding", Description: "Write code"},
		{Name: "testing", Description: "Run tests"},
	}
	result := FormatSkillInstructions(skills)
	if !strings.Contains(result, "**coding**") {
		t.Error("should contain skill name")
	}
	if !strings.Contains(result, "Write code") {
		t.Error("should contain description")
	}
	if !strings.Contains(result, "**testing**") {
		t.Error("should contain second skill")
	}
}

func TestFormatSkillInstructions_Empty(t *testing.T) {
	result := FormatSkillInstructions(nil)
	if result != "" {
		t.Error("empty skills should produce empty string")
	}
}

// --- SkillViewerTool ---

func TestSkillViewerTool_Found(t *testing.T) {
	skills := []Skill{
		{Name: "coding", Markdown: "Step 1: Write tests\nStep 2: Write code"},
	}
	viewer := NewSkillViewerTool(skills)

	resp, err := viewer.Execute(context.Background(), map[string]any{"skill": "coding"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != "success" {
		t.Error("should succeed")
	}
	if len(resp.Content) == 0 {
		t.Fatal("response should have content")
	}
}

func TestSkillViewerTool_NotFound(t *testing.T) {
	viewer := NewSkillViewerTool(nil)

	resp, err := viewer.Execute(context.Background(), map[string]any{"skill": "nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != "error" {
		t.Error("should return error for unknown skill")
	}
}

func TestSkillViewerTool_EmptyName(t *testing.T) {
	viewer := NewSkillViewerTool(nil)

	resp, _ := viewer.Execute(context.Background(), map[string]any{})
	if resp.State != "error" {
		t.Error("should return error for missing skill name")
	}
}

func TestSkillViewerTool_Name(t *testing.T) {
	viewer := NewSkillViewerTool(nil)
	if viewer.Name() != "Skill" {
		t.Errorf("expected tool name 'Skill', got %s", viewer.Name())
	}
}
