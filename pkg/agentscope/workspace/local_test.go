package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEnhancedLocalWorkspace_MCP(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewEnhancedLocalWorkspace(LocalConfig{BasePath: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Initially empty
	mcps, err := ws.ListMCPs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(mcps) != 0 {
		t.Fatalf("expected 0 MCPs, got %d", len(mcps))
	}

	// Add MCP
	err = ws.AddMCP(ctx, "test-server", map[string]any{
		"command": "npx",
		"args":    []any{"test-mcp"},
	})
	if err != nil {
		t.Fatal(err)
	}

	mcps, err = ws.ListMCPs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(mcps) != 1 {
		t.Fatalf("expected 1 MCP, got %d", len(mcps))
	}
	if mcps[0].Name != "test-server" {
		t.Errorf("name = %q", mcps[0].Name)
	}

	// Add another MCP
	err = ws.AddMCP(ctx, "another-server", map[string]any{
		"url": "http://localhost:3000",
	})
	if err != nil {
		t.Fatal(err)
	}

	mcps, err = ws.ListMCPs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(mcps) != 2 {
		t.Fatalf("expected 2 MCPs, got %d", len(mcps))
	}

	// Remove MCP
	err = ws.RemoveMCP(ctx, "test-server")
	if err != nil {
		t.Fatal(err)
	}

	mcps, err = ws.ListMCPs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(mcps) != 1 {
		t.Fatalf("expected 1 MCP after removal, got %d", len(mcps))
	}

	// Remove nonexistent
	err = ws.RemoveMCP(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error removing nonexistent MCP")
	}

	// Verify persistence
	data, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty .mcp.json")
	}
}

func TestEnhancedLocalWorkspace_MCP_EmptyName(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewEnhancedLocalWorkspace(LocalConfig{BasePath: dir})
	if err != nil {
		t.Fatal(err)
	}
	err = ws.AddMCP(context.Background(), "", map[string]any{})
	if err == nil {
		t.Error("expected error for empty MCP name")
	}
}

func TestEnhancedLocalWorkspace_Skills(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewEnhancedLocalWorkspace(LocalConfig{BasePath: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Initially empty
	skills, err := ws.ListSkills(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 0 {
		t.Fatalf("expected 0 skills, got %d", len(skills))
	}

	// Add skill
	err = ws.AddSkill(ctx, "/path/to/my_skill.md")
	if err != nil {
		t.Fatal(err)
	}

	skills, err = ws.ListSkills(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if skills[0].Name != "my_skill" {
		t.Errorf("name = %q, want %q", skills[0].Name, "my_skill")
	}
	if skills[0].Path != "/path/to/my_skill.md" {
		t.Errorf("path = %q", skills[0].Path)
	}

	// Duplicate
	err = ws.AddSkill(ctx, "/other/my_skill.md")
	if err == nil {
		t.Error("expected error for duplicate skill name")
	}

	// Remove
	err = ws.RemoveSkill(ctx, "my_skill")
	if err != nil {
		t.Fatal(err)
	}

	skills, err = ws.ListSkills(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 0 {
		t.Fatalf("expected 0 skills after removal, got %d", len(skills))
	}

	// Remove nonexistent
	err = ws.RemoveSkill(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error removing nonexistent skill")
	}
}

func TestEnhancedLocalWorkspace_Skills_EmptyPath(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewEnhancedLocalWorkspace(LocalConfig{BasePath: dir})
	if err != nil {
		t.Fatal(err)
	}
	err = ws.AddSkill(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty skill path")
	}
}

func TestEnhancedLocalWorkspace_GetInstructions(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewEnhancedLocalWorkspace(LocalConfig{BasePath: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// No instructions file
	instructions, err := ws.GetInstructions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if instructions != "" {
		t.Errorf("expected empty instructions, got %q", instructions)
	}

	// Create CLAUDE.md
	_ = os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# Instructions\nDo good things."), 0o644)

	instructions, err = ws.GetInstructions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if instructions != "# Instructions\nDo good things." {
		t.Errorf("instructions = %q", instructions)
	}
}

func TestEnhancedLocalWorkspace_GetInstructions_README(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewEnhancedLocalWorkspace(LocalConfig{BasePath: dir})
	if err != nil {
		t.Fatal(err)
	}

	// Only README.md
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("# README"), 0o644)

	instructions, err := ws.GetInstructions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instructions != "# README" {
		t.Errorf("instructions = %q", instructions)
	}
}

func TestEnhancedLocalWorkspace_ImplementsWorkspace(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewEnhancedLocalWorkspace(LocalConfig{BasePath: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Verify it still works as a regular Workspace
	err = ws.WriteFile(ctx, "test.txt", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := ws.ReadFile(ctx, "test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Errorf("ReadFile = %q", string(data))
	}
}
