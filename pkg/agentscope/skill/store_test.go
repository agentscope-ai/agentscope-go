package skill

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSeedSkill writes a skill directory under the seed template.
func writeSeedSkill(t *testing.T, root, dirName, name string) {
	t.Helper()
	dir := filepath.Join(root, SkillsDirName, SeedPartitionName, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + name + " desc\n---\n\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStore_EquipsPartitionFromSeedOnce(t *testing.T) {
	root := t.TempDir()
	writeSeedSkill(t, root, "seeded", "seeded")

	s := NewStore(root)
	skills, err := s.List("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "seeded" {
		t.Fatalf("first list must equip from seed, got %+v", skills)
	}

	// Deleting the equipped copy sticks: the seed must not come back.
	if err := s.Remove("alice", "seeded"); err != nil {
		t.Fatal(err)
	}
	s2 := NewStore(root) // fresh store = fresh process perspective
	skills, err = s2.List("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 0 {
		t.Fatalf("deleted seed skill must stay deleted, got %+v", skills)
	}

	// A different agent still equips its own copy from the seed.
	other, err := s2.List("bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].Name != "seeded" {
		t.Fatalf("bob must equip from seed independently, got %+v", other)
	}
}

func TestStore_ListOnlyOneLevelDeep(t *testing.T) {
	root := t.TempDir()
	partition := filepath.Join(root, SkillsDirName, "alice")
	// A real skill.
	top := filepath.Join(partition, "real")
	if err := os.MkdirAll(top, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, "SKILL.md"),
		[]byte("---\nname: real\ndescription: d\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A SKILL.md shipped inside a skill's subfolder — not a second skill.
	nested := filepath.Join(top, "assets")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "SKILL.md"),
		[]byte("---\nname: nested\ndescription: d\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}

	skills, err := NewStore(root).List("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "real" {
		t.Fatalf("only the top-level skill counts, got %+v", skills)
	}
}

func TestStore_AddContentAndList(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)

	dirName, err := s.Add("alice", "My Skill!", "", "", "Do the thing.")
	if err != nil {
		t.Fatal(err)
	}
	if dirName != "my-skill" {
		t.Errorf("dir name must be slugified, got %q", dirName)
	}

	skills, err := s.List("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %+v", skills)
	}
	sk := skills[0]
	if sk.Name != "My Skill!" || sk.Description != "My Skill!" || !strings.Contains(sk.Markdown, "Do the thing.") {
		t.Errorf("round-trip wrong: %+v", sk)
	}

	// Duplicate name collides.
	if _, err := s.Add("alice", "My Skill!", "", "", "again"); err == nil {
		t.Error("duplicate add must fail")
	}
	// Validation.
	if _, err := s.Add("alice", "", "", "", "x"); err == nil {
		t.Error("empty name must fail")
	}
	if _, err := s.Add("alice", "x", "", "", "  "); err == nil {
		t.Error("empty instructions must fail")
	}
}

func TestStore_AddDir(t *testing.T) {
	root := t.TempDir()
	src := t.TempDir()
	srcDir := filepath.Join(src, "pack")
	if err := os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "SKILL.md"),
		[]byte("---\nname: pack\ndescription: d\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "sub", "extra.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewStore(root)
	if err := s.AddDir("alice", srcDir); err != nil {
		t.Fatal(err)
	}
	// Nested file copied along.
	if _, err := os.Stat(filepath.Join(s.PartitionPath("alice"), "pack", "sub", "extra.txt")); err != nil {
		t.Errorf("nested file missing: %v", err)
	}
	// Duplicate rejected.
	if err := s.AddDir("alice", srcDir); err == nil {
		t.Error("duplicate AddDir must fail")
	}
	// Missing SKILL.md rejected.
	if err := s.AddDir("alice", t.TempDir()); err == nil {
		t.Error("AddDir without SKILL.md must fail")
	}
}

func TestStore_RemoveAndPurge(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	if _, err := s.Add("alice", "one", "", "", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("alice", "two", "", "", "b"); err != nil {
		t.Fatal(err)
	}

	if err := s.Remove("alice", "one"); err != nil {
		t.Fatal(err)
	}
	skills, _ := s.List("alice")
	if len(skills) != 1 || skills[0].Name != "two" {
		t.Fatalf("remove wrong: %+v", skills)
	}
	if err := s.Remove("alice", "ghost"); err == nil {
		t.Error("removing a missing skill must fail")
	}

	if err := s.PurgeAgent("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.PartitionPath("alice")); !os.IsNotExist(err) {
		t.Error("partition must be gone after purge")
	}
}

func TestStore_RejectsEscapingAgentIDs(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	for _, id := range []string{".", "..", ".seed", "a/b", "a\\b", ".hidden"} {
		if _, err := s.List(id); err == nil {
			t.Errorf("agent id %q must be rejected", id)
		}
		if err := s.PurgeAgent(id); err == nil {
			t.Errorf("purge of %q must be rejected", id)
		}
	}
}

func TestStore_MigrateLegacyLayout(t *testing.T) {
	root := t.TempDir()
	// Legacy layout: a skill directly under skills/.
	legacy := filepath.Join(root, SkillsDirName, "legacy-skill")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "SKILL.md"),
		[]byte("---\nname: legacy\ndescription: d\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A partition that must stay put.
	if err := os.MkdirAll(filepath.Join(root, SkillsDirName, "alice", "keep"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := NewStore(root)
	if err := s.MigrateLegacy(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, SkillsDirName, SeedPartitionName, "legacy-skill", "SKILL.md")); err != nil {
		t.Errorf("legacy skill must move into the seed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, SkillsDirName, "alice", "keep")); err != nil {
		t.Errorf("partitions must survive migration: %v", err)
	}

	// Idempotent: second run is a no-op.
	if err := s.MigrateLegacy(); err != nil {
		t.Fatal(err)
	}

	// A fresh agent equips the migrated skill from the seed.
	skills, err := NewStore(root).List("carol")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "legacy" {
		t.Fatalf("migrated skill must reach new agents, got %+v", skills)
	}
}

func TestSkillDirName(t *testing.T) {
	cases := map[string]string{
		"My Skill!":  "my-skill",
		"already-ok": "already-ok",
		"  spaced  ": "spaced",
		"///":        "skill",
		"Übung":      "bung", // non-ASCII letters map to separators
	}
	for in, want := range cases {
		if got := SkillDirName(in); got != want {
			t.Errorf("SkillDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilterByName(t *testing.T) {
	skills := []Skill{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	if got := FilterByName(skills, nil); len(got) != 3 {
		t.Errorf("empty filter must keep all, got %v", got)
	}
	got := FilterByName(skills, []string{"c", "a"})
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Errorf("filter must preserve skill order, got %+v", got)
	}
}

func TestStore_SentinelErrors(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	if _, err := s.Add("alice", "one", "", "", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("alice", "one", "", "", "y"); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("duplicate add must be ErrAlreadyExists, got %v", err)
	}
	if err := s.Remove("alice", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing remove must be ErrNotFound, got %v", err)
	}
	if _, err := s.List("../esc"); !errors.Is(err, ErrInvalidAgentID) {
		t.Errorf("escaping agent id must be ErrInvalidAgentID, got %v", err)
	}
	if err := s.PurgeAgent("a\\b"); !errors.Is(err, ErrInvalidAgentID) {
		t.Errorf("escaping purge must be ErrInvalidAgentID, got %v", err)
	}
}

func TestStore_MigrateLegacyIndexFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, SkillsDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, SkillsDirName, ".skills"), []byte("legacy index"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewStore(root)
	if err := s.MigrateLegacy(); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, SkillsDirName, SeedPartitionName, ".index")
	content, err := os.ReadFile(moved)
	if err != nil || string(content) != "legacy index" {
		t.Errorf(".skills must migrate to .seed/.index, err=%v content=%q", err, content)
	}
}

func TestStore_YAMLRoundTripWithSpecialChars(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	name := `Tricky "name"`
	desc := "line1\r\nline2\ttabbed \\ backslash"
	if _, err := s.Add("alice", name, desc, "", "body"); err != nil {
		t.Fatal(err)
	}
	skills, err := s.List("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %+v", skills)
	}
	if skills[0].Name != name || skills[0].Description != desc {
		t.Errorf("special chars must round-trip, got %q / %q", skills[0].Name, skills[0].Description)
	}
}

func TestStore_DelimiterMetadataRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	const description = "Convert foo---bar safely"
	const category = "editing---text"
	const body = "Use foo---bar.\n\n---\n\nKeep the horizontal rule."
	if _, err := s.Add("alice", "convert", description, category, body); err != nil {
		t.Fatal(err)
	}
	skills, err := s.List("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Errorf("expected the added skill to be listed, got %d skills", len(skills))
	} else if sk := skills[0]; sk.Name != "convert" || sk.Description != description || sk.Category != category || sk.Markdown != body {
		t.Errorf("unexpected round-trip skill: %+v", sk)
	}
	if err := s.Remove("alice", "convert"); err != nil {
		t.Fatal(err)
	}
	skills, err = s.List("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 0 {
		t.Errorf("expected no skills after removal, got %d", len(skills))
	}
}

func TestStore_AddRejectsInvalidInputSentinel(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.Add("alice", "", "", "", "x"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty name must be ErrInvalidInput, got %v", err)
	}
	if _, err := s.Add("alice", "n", "", "", "  "); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty instructions must be ErrInvalidInput, got %v", err)
	}
}

func TestStore_YAMLRoundTripWithC0Controls(t *testing.T) {
	s := NewStore(t.TempDir())
	desc := "bell\x07 vt\x0b ff\x0c nul-ish\x1f end"
	if _, err := s.Add("alice", "ctrl", desc, "", "body"); err != nil {
		t.Fatal(err)
	}
	skills, err := s.List("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("C0 controls must not break parsing, got %d skills", len(skills))
	}
	if skills[0].Description != desc {
		t.Errorf("C0 description must round-trip, got %q", skills[0].Description)
	}
}
