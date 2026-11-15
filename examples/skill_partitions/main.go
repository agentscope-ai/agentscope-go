package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/skill"
)

// This example demonstrates per-agent workspace skill isolation (Phase 3,
// Python #2283 semantics):
//
//	skills/.seed/<skill-dir>/SKILL.md        seed template
//	skills/<agent_id>/<skill-dir>/SKILL.md   one partition per agent
//
//   - An agent's first skill access equips its partition from .seed
//     exactly once (a deleted seed skill stays deleted).
//   - The pre-partition layout (skill dirs directly under skills/) is
//     migrated into the seed idempotently.
//   - skill.SkillManager gains matching in-memory agent partitions.
//
// No API keys or network needed.

func main() {
	root, err := os.MkdirTemp("", "agentscope-skills-*")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer os.RemoveAll(root)

	// ---------- Seed template ----------
	writeSkill := func(dir, name, desc, body string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			panic(err)
		}
		content := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
			panic(err)
		}
	}
	writeSkill(filepath.Join(root, skill.SkillsDirName, skill.SeedPartitionName, "code-review"),
		"code-review", "Review a diff for bugs and style", "Check error handling, races, and naming.")

	store := skill.NewStore(root)

	// ---------- First access equips from the seed ----------
	fmt.Println("=== alice's first list (equipped from .seed) ===")
	printSkills(store, "alice")

	// ---------- Content-based add ----------
	if _, err := store.Add("alice", "Deploy Runbook", "How we deploy the service", "ops",
		"1. Freeze after Thursday.\n2. Run the pipeline.\n3. Watch the dashboard."); err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Println("=== alice after Add('Deploy Runbook') ===")
	printSkills(store, "alice")

	// ---------- bob equips his own copy independently ----------
	fmt.Println("=== bob's first list (his own equip) ===")
	printSkills(store, "bob")

	// ---------- alice deletes her seeded skill — it stays deleted ----------
	if err := store.Remove("alice", "code-review"); err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Println("=== alice after removing 'code-review' (seed copy stays gone) ===")
	printSkills(store, "alice")

	// ---------- In-memory manager partitions ----------
	mgr := skill.NewSkillManager()
	if err := mgr.LoadAgentFromStore(store, "alice"); err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Println("=== SkillManager.FormatInstructionsForAgent(\"alice\") ===")
	fmt.Println(mgr.FormatInstructionsForAgent("alice"))

	// ---------- Purge ----------
	if err := store.PurgeAgent("alice"); err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Println("=== alice after PurgeAgent (next access re-equips from seed) ===")
	printSkills(store, "alice")

	// ---------- Legacy layout migration ----------
	legacyRoot, err := os.MkdirTemp("", "agentscope-skills-legacy-*")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer os.RemoveAll(legacyRoot)
	// Old layout: a skill directory directly under skills/.
	writeSkill(filepath.Join(legacyRoot, skill.SkillsDirName, "old-helper"),
		"old-helper", "A pre-partition skill", "Legacy body.")

	legacyStore := skill.NewStore(legacyRoot)
	if err := legacyStore.MigrateLegacy(); err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Println("=== legacy layout migrated into .seed ===")
	fmt.Println("  .seed now holds:", filepath.Join(skill.SkillsDirName, skill.SeedPartitionName, "old-helper"))
	printSkills(legacyStore, "carol")
}

func printSkills(store *skill.Store, agentID string) {
	skills, err := store.List(agentID)
	if err != nil {
		fmt.Println("  Error:", err)
		return
	}
	if len(skills) == 0 {
		fmt.Println("  (no skills)")
		return
	}
	for _, s := range skills {
		fmt.Printf("  %-15s %s\n", s.Name, s.Description)
	}
}
