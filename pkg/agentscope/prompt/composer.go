// Package prompt provides composable system prompt assembly from multiple
// named sections and built-in section providers.
package prompt

import (
	"sort"
	"strings"
)

// PromptSection represents a named section of the system prompt.
type PromptSection struct {
	Name     string // e.g. "base", "project_config", "skills", "environment"
	Priority int    // lower = earlier in output
	Content  string
}

// PromptComposer assembles system prompts from multiple sections. Sections are
// keyed by name; adding a section with an existing name replaces it.
type PromptComposer struct {
	sections map[string]PromptSection
}

// NewPromptComposer creates an empty composer.
func NewPromptComposer() *PromptComposer {
	return &PromptComposer{sections: make(map[string]PromptSection)}
}

// Add inserts or replaces a section, keyed by its Name.
func (c *PromptComposer) Add(section PromptSection) {
	c.sections[section.Name] = section
}

// SetSection replaces the content of an existing section or adds a new one. The
// priority of an existing section is preserved; a new section gets priority 0.
func (c *PromptComposer) SetSection(name, content string) {
	if s, ok := c.sections[name]; ok {
		s.Content = content
		c.sections[name] = s
		return
	}
	c.sections[name] = PromptSection{Name: name, Content: content}
}

// RemoveSection deletes a section by name. No-op if it does not exist.
func (c *PromptComposer) RemoveSection(name string) {
	delete(c.sections, name)
}

// Compose sorts sections by priority (then name for stability), drops empty
// content, and joins them with blank lines.
func (c *PromptComposer) Compose() string {
	sections := c.Sections()
	parts := make([]string, 0, len(sections))
	for _, s := range sections {
		if strings.TrimSpace(s.Content) == "" {
			continue
		}
		parts = append(parts, s.Content)
	}
	return strings.Join(parts, "\n\n")
}

// Sections returns a copy of all sections sorted by priority, then name.
func (c *PromptComposer) Sections() []PromptSection {
	out := make([]PromptSection, 0, len(c.sections))
	for _, s := range c.sections {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}
