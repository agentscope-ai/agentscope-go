package skill

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSkillManagerRegisterAndGet(t *testing.T) {
	m := NewSkillManager()
	s := &Skill{Name: "alpha", Description: "first"}
	m.Register(s)

	got, ok := m.Get("alpha")
	if !ok {
		t.Fatal("expected to find skill alpha")
	}
	if got != s {
		t.Errorf("expected same skill pointer back")
	}
	if m.Len() != 1 {
		t.Errorf("expected len 1, got %d", m.Len())
	}
}

func TestSkillManagerGetNotFound(t *testing.T) {
	m := NewSkillManager()
	if _, ok := m.Get("missing"); ok {
		t.Error("expected missing skill to not be found")
	}
}

func TestSkillManagerRegisterNilAndEmpty(t *testing.T) {
	m := NewSkillManager()
	m.Register(nil)
	m.Register(&Skill{Name: ""})
	if m.Len() != 0 {
		t.Errorf("expected len 0 after nil/empty registrations, got %d", m.Len())
	}
}

func TestSkillManagerList(t *testing.T) {
	m := NewSkillManager()
	m.Register(&Skill{Name: "gamma"})
	m.Register(&Skill{Name: "alpha"})
	m.Register(&Skill{Name: "beta"})

	list := m.List()
	want := []string{"alpha", "beta", "gamma"}
	if len(list) != len(want) {
		t.Fatalf("expected %d skills, got %d", len(want), len(list))
	}
	for i, name := range want {
		if list[i].Name != name {
			t.Errorf("expected list[%d]=%s, got %s", i, name, list[i].Name)
		}
	}
}

func TestSkillManagerListByCategory(t *testing.T) {
	m := NewSkillManager()
	m.Register(&Skill{Name: "b", Category: "dev"})
	m.Register(&Skill{Name: "a", Category: "dev"})
	m.Register(&Skill{Name: "c", Category: "ops"})

	dev := m.ListByCategory("dev")
	if len(dev) != 2 {
		t.Fatalf("expected 2 dev skills, got %d", len(dev))
	}
	if dev[0].Name != "a" || dev[1].Name != "b" {
		t.Errorf("expected sorted [a b], got [%s %s]", dev[0].Name, dev[1].Name)
	}

	ops := m.ListByCategory("ops")
	if len(ops) != 1 || ops[0].Name != "c" {
		t.Errorf("expected [c], got %v", ops)
	}

	if got := m.ListByCategory("none"); len(got) != 0 {
		t.Errorf("expected no skills for unknown category, got %d", len(got))
	}
}

func TestSkillManagerLoadFromDir(t *testing.T) {
	root := t.TempDir()
	writeSkillMD(t, filepath.Join(root, "one"), "one", "first skill", "body one")
	writeSkillMD(t, filepath.Join(root, "two"), "two", "second skill", "body two")

	m := NewSkillManager()
	if err := m.LoadFromDir(root); err != nil {
		t.Fatal(err)
	}
	if m.Len() != 2 {
		t.Fatalf("expected 2 skills loaded, got %d", m.Len())
	}
	if _, ok := m.Get("one"); !ok {
		t.Error("expected skill one to be loaded")
	}
	if _, ok := m.Get("two"); !ok {
		t.Error("expected skill two to be loaded")
	}
}

func TestSkillManagerFormatInstructions(t *testing.T) {
	m := NewSkillManager()
	m.Register(&Skill{Name: "alpha", Description: "does alpha things"})

	out := m.FormatInstructions()
	if !strings.Contains(out, "alpha") {
		t.Errorf("expected instructions to mention alpha, got %q", out)
	}
	if !strings.Contains(out, "does alpha things") {
		t.Errorf("expected instructions to mention description, got %q", out)
	}
}

func TestSkillManagerReplaceExisting(t *testing.T) {
	m := NewSkillManager()
	m.Register(&Skill{Name: "alpha", Description: "old"})
	m.Register(&Skill{Name: "alpha", Description: "new"})

	if m.Len() != 1 {
		t.Fatalf("expected len 1 after replace, got %d", m.Len())
	}
	got, _ := m.Get("alpha")
	if got.Description != "new" {
		t.Errorf("expected description=new, got %s", got.Description)
	}
}

func TestSkillManagerConcurrency(t *testing.T) {
	m := NewSkillManager()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			name := "skill" + string(rune('A'+n%26))
			m.Register(&Skill{Name: name, Category: "cat"})
			m.Get(name)
			m.List()
			m.ListByCategory("cat")
			m.Len()
			m.FormatInstructions()
		}(i)
	}
	wg.Wait()
}

func TestSkillManager_AgentPartitions(t *testing.T) {
	m := NewSkillManager()

	// Global registry stays independent of partitions.
	m.Register(&Skill{Name: "global", Description: "g"})

	m.RegisterForAgent("alice", &Skill{Name: "alpha", Description: "a"})
	m.RegisterForAgent("alice", &Skill{Name: "beta", Description: "b"})
	m.RegisterForAgent("bob", &Skill{Name: "gamma", Description: "c"})

	if got := m.List(); len(got) != 1 || got[0].Name != "global" {
		t.Errorf("global list must not see partitions, got %+v", got)
	}
	alice := m.ListForAgent("alice")
	if len(alice) != 2 || alice[0].Name != "alpha" || alice[1].Name != "beta" {
		t.Errorf("alice partition wrong: %+v", alice)
	}
	if s, ok := m.GetForAgent("alice", "beta"); !ok || s.Description != "b" {
		t.Errorf("GetForAgent wrong: %v %v", s, ok)
	}
	if _, ok := m.GetForAgent("alice", "gamma"); ok {
		t.Error("partitions must be isolated")
	}

	// Replace-in-partition.
	m.RegisterForAgent("alice", &Skill{Name: "alpha", Description: "v2"})
	if s, _ := m.GetForAgent("alice", "alpha"); s.Description != "v2" {
		t.Errorf("re-register must replace: %+v", s)
	}

	m.PurgeAgent("alice")
	if got := m.ListForAgent("alice"); len(got) != 0 {
		t.Errorf("purged partition must be empty, got %+v", got)
	}
	if got := m.ListForAgent("bob"); len(got) != 1 {
		t.Errorf("other partitions must survive a purge, got %+v", got)
	}
}

func TestSkillManager_LoadAgentFromStore(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	if _, err := store.Add("alice", "loaded", "", "", "instructions"); err != nil {
		t.Fatal(err)
	}

	m := NewSkillManager()
	if err := m.LoadAgentFromStore(store, "alice"); err != nil {
		t.Fatal(err)
	}
	skills := m.ListForAgent("alice")
	if len(skills) != 1 || skills[0].Name != "loaded" {
		t.Fatalf("load from store wrong: %+v", skills)
	}
	if got := m.FormatInstructionsForAgent("alice"); !strings.Contains(got, "loaded") {
		t.Errorf("agent instructions must list the skill, got %q", got)
	}

	// Reload replaces the partition content.
	if err := store.Remove("alice", "loaded"); err != nil {
		t.Fatal(err)
	}
	if err := m.LoadAgentFromStore(store, "alice"); err != nil {
		t.Fatal(err)
	}
	if got := m.ListForAgent("alice"); len(got) != 0 {
		t.Errorf("reload must reflect removal, got %+v", got)
	}

	if err := m.LoadAgentFromStore(nil, "alice"); err == nil {
		t.Error("nil store must error")
	}
}
