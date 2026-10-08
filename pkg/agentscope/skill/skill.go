package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"gopkg.in/yaml.v3"
)

// Skill represents a loadable instruction document. Skills are not callable
// tools — they are markdown documents that the agent reads (via the
// SkillViewerTool) and follows using actual tools.
type Skill struct {
	Name        string  // from SKILL.md frontmatter
	Description string  // from SKILL.md frontmatter
	Category    string  // optional grouping category from SKILL.md frontmatter
	Dir         string  // directory containing SKILL.md
	Markdown    string  // body content (after frontmatter)
	ModTime     float64 // file mtime for cache invalidation
}

// SkillLoader discovers and loads skills.
type SkillLoader interface {
	LoadSkills() ([]Skill, error)
}

// LocalSkillLoader loads skills from a directory by scanning for SKILL.md files.
type LocalSkillLoader struct {
	Dir        string
	ScanSubdir bool

	mu    sync.Mutex
	cache map[string]*Skill
}

// NewLocalSkillLoader creates a loader that reads skills from the given directory.
// If scanSubdir is true, it recursively scans subdirectories.
func NewLocalSkillLoader(dir string, scanSubdir bool) *LocalSkillLoader {
	return &LocalSkillLoader{
		Dir:        dir,
		ScanSubdir: scanSubdir,
		cache:      make(map[string]*Skill),
	}
}

func (l *LocalSkillLoader) LoadSkills() ([]Skill, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var dirs []string
	if l.ScanSubdir {
		err := filepath.WalkDir(l.Dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				skillPath := filepath.Join(path, "SKILL.md")
				if _, err := os.Stat(skillPath); err == nil {
					dirs = append(dirs, path)
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("skill: walk %s: %w", l.Dir, err)
		}
	} else {
		skillPath := filepath.Join(l.Dir, "SKILL.md")
		if _, err := os.Stat(skillPath); err == nil {
			dirs = append(dirs, l.Dir)
		}
	}

	var skills []Skill
	for _, dir := range dirs {
		s, err := l.loadSingle(dir)
		if err != nil {
			continue
		}
		skills = append(skills, *s)
	}
	return skills, nil
}

func (l *LocalSkillLoader) loadSingle(dir string) (*Skill, error) {
	path := filepath.Join(dir, "SKILL.md")
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	mtime := float64(info.ModTime().UnixNano()) / 1e9
	if cached, ok := l.cache[dir]; ok && cached.ModTime == mtime {
		return cached, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	s, err := parseSKILLMD(data, dir, mtime)
	if err != nil {
		return nil, err
	}

	l.cache[dir] = s
	return s, nil
}

// parseSKILLMD parses a SKILL.md file with YAML frontmatter.
func parseSKILLMD(data []byte, dir string, mtime float64) (*Skill, error) {
	content := strings.TrimSpace(string(data))

	if !strings.HasPrefix(content, "---") {
		return nil, fmt.Errorf("skill: SKILL.md missing frontmatter in %s", dir)
	}

	var frontmatter, markdown string
	closed := false
	// A closing delimiter must occupy an unindented line. Inline and indented
	// occurrences of "---" can be part of YAML scalars.
	for offset := 3; offset < len(content); {
		line, rest, hasNewline := strings.Cut(content[offset:], "\n")
		if strings.TrimRight(line, " \t\r") == "---" {
			frontmatter, markdown = content[3:offset], rest
			closed = true
			break
		}
		if !hasNewline {
			break
		}
		offset += len(line) + 1
	}
	if !closed {
		return nil, fmt.Errorf("skill: invalid frontmatter in %s", dir)
	}

	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Category    string `yaml:"category"`
	}
	if err := yaml.Unmarshal([]byte(frontmatter), &fm); err != nil {
		return nil, fmt.Errorf("skill: parse frontmatter in %s: %w", dir, err)
	}
	if fm.Name == "" || fm.Description == "" {
		return nil, fmt.Errorf("skill: name and description required in %s", dir)
	}

	body := strings.TrimSpace(markdown)

	return &Skill{
		Name:        fm.Name,
		Description: fm.Description,
		Category:    fm.Category,
		Dir:         dir,
		Markdown:    body,
		ModTime:     mtime,
	}, nil
}

// FormatSkillInstructions generates a system prompt section listing available
// skills. The agent uses the Skill viewer tool to read full instructions.
func FormatSkillInstructions(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n\n## Available Skills\n\n")
	sb.WriteString("Skills are instruction documents, NOT tools. To use a skill, ")
	sb.WriteString("call the `Skill` tool with the skill name to read its full instructions, ")
	sb.WriteString("then follow those instructions using the available tools.\n\n")
	for _, s := range skills {
		sb.WriteString(fmt.Sprintf("- **%s**: %s\n", s.Name, s.Description))
	}
	return sb.String()
}

// --- SkillViewerTool ---

var skillViewerSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"skill": {
			"type": "string",
			"description": "The exact name of the skill to view."
		}
	},
	"required": ["skill"]
}`)

// SkillViewerTool allows the agent to read the full markdown instructions
// of a loaded skill by name.
type SkillViewerTool struct {
	tool.BaseTool
	skills map[string]*Skill
}

// NewSkillViewerTool creates a viewer tool from a slice of skills.
func NewSkillViewerTool(skills []Skill) *SkillViewerTool {
	m := make(map[string]*Skill, len(skills))
	for i := range skills {
		m[skills[i].Name] = &skills[i]
	}
	return &SkillViewerTool{
		BaseTool: tool.BaseTool{
			ToolName:        "Skill",
			ToolDescription: "View the full instructions of a skill by name. Call this before using a skill.",
			ToolSchema:      skillViewerSchema,
			ConcurrencySafe: true,
			ReadOnly:        true,
		},
		skills: m,
	}
}

func (t *SkillViewerTool) Execute(_ context.Context, input map[string]any) (*tool.ToolResponse, error) {
	name, _ := input["skill"].(string)
	if name == "" {
		return tool.NewErrorResponse(fmt.Errorf("skill name is required")), nil
	}

	s, ok := t.skills[name]
	if !ok {
		return tool.NewErrorResponse(fmt.Errorf("skill %q not found", name)), nil
	}

	return tool.NewTextResponse(s.Markdown), nil
}

func (t *SkillViewerTool) CheckPermissions(_ map[string]any, _ *permission.Context) permission.Decision {
	return permission.Decision{
		Behavior: permission.BehaviorAllow,
		Message:  "auto-allowed: skill viewer",
	}
}
