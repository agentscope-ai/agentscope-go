package skill

import (
	"fmt"
	"sort"
	"sync"
)

// SkillManager is a concurrency-safe registry of loaded skills keyed by
// name, plus optional per-agent partitions mirroring the workspace skill
// layout (skills/<agent_id>/, Python #2283). The unpartitioned registry
// (Register/List/Get) is global and unchanged; partition methods take an
// agent ID ("" is the default partition).
type SkillManager struct {
	mu      sync.RWMutex
	skills  map[string]*Skill
	byAgent map[string]map[string]*Skill
}

// NewSkillManager creates an empty SkillManager.
func NewSkillManager() *SkillManager {
	return &SkillManager{
		skills:  make(map[string]*Skill),
		byAgent: make(map[string]map[string]*Skill),
	}
}

// Register adds a skill to the manager. Nil skills or skills with an empty
// name are ignored. An existing skill with the same name is replaced.
func (m *SkillManager) Register(s *Skill) {
	if s == nil || s.Name == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.skills[s.Name] = s
}

// Get returns the skill with the given name, if present.
func (m *SkillManager) Get(name string) (*Skill, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.skills[name]
	return s, ok
}

// List returns all registered skills sorted by name.
func (m *SkillManager) List() []*Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Skill, 0, len(m.skills))
	for _, s := range m.skills {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

// ListByCategory returns all registered skills in the given category, sorted by name.
func (m *SkillManager) ListByCategory(category string) []*Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Skill
	for _, s := range m.skills {
		if s.Category == category {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

// LoadFromDir loads all skills found under dir (recursively) and registers them.
func (m *SkillManager) LoadFromDir(dir string) error {
	loader := NewLocalSkillLoader(dir, true)
	skills, err := loader.LoadSkills()
	if err != nil {
		return fmt.Errorf("skill: load from dir %s: %w", dir, err)
	}
	for i := range skills {
		s := skills[i]
		m.Register(&s)
	}
	return nil
}

// FormatInstructions renders the system prompt section listing registered skills.
func (m *SkillManager) FormatInstructions() string {
	list := m.List()
	skills := make([]Skill, 0, len(list))
	for _, s := range list {
		skills = append(skills, *s)
	}
	return FormatSkillInstructions(skills)
}

// Len returns the number of registered skills.
func (m *SkillManager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.skills)
}

// RegisterForAgent adds a skill to one agent's partition. Nil skills or
// skills with an empty name are ignored; an existing skill with the same
// name in the same partition is replaced.
func (m *SkillManager) RegisterForAgent(agentID string, s *Skill) {
	if s == nil || s.Name == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	part := m.byAgent[agentID]
	if part == nil {
		part = make(map[string]*Skill)
		m.byAgent[agentID] = part
	}
	part[s.Name] = s
}

// GetForAgent returns the skill from one agent's partition, if present.
func (m *SkillManager) GetForAgent(agentID, name string) (*Skill, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.byAgent[agentID][name]
	return s, ok
}

// ListForAgent returns the skills in one agent's partition sorted by name.
func (m *SkillManager) ListForAgent(agentID string) []*Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	part := m.byAgent[agentID]
	out := make([]*Skill, 0, len(part))
	for _, s := range part {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

// PurgeAgent removes one agent's whole in-memory partition.
func (m *SkillManager) PurgeAgent(agentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byAgent, agentID)
}

// LoadAgentFromStore loads one agent's workspace partition into the
// manager, replacing any previous in-memory copy of that partition.
func (m *SkillManager) LoadAgentFromStore(store *Store, agentID string) error {
	if store == nil {
		return fmt.Errorf("skill: store is required")
	}
	skills, err := store.List(agentID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	part := make(map[string]*Skill, len(skills))
	for i := range skills {
		part[skills[i].Name] = &skills[i]
	}
	m.byAgent[agentID] = part
	m.mu.Unlock()
	return nil
}

// FormatInstructionsForAgent renders the system prompt section listing one
// agent's partitioned skills.
func (m *SkillManager) FormatInstructionsForAgent(agentID string) string {
	list := m.ListForAgent(agentID)
	skills := make([]Skill, 0, len(list))
	for _, s := range list {
		skills = append(skills, *s)
	}
	return FormatSkillInstructions(skills)
}

// FilterByName returns the skills whose names appear in names, preserving
// skill order. An empty name list filters nothing (all skills active).
func FilterByName(skills []Skill, names []string) []Skill {
	if len(names) == 0 {
		return skills
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	out := make([]Skill, 0, len(names))
	for i := range skills {
		if want[skills[i].Name] {
			out = append(out, skills[i])
		}
	}
	return out
}
