package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/fsutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// MCPInfo describes a registered MCP server configuration.
type MCPInfo struct {
	Name   string         `json:"name"`
	Config map[string]any `json:"config"`
}

// SkillInfo describes a registered skill.
type SkillInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// ManagedWorkspace extends Workspace with MCP server and skill management.
type ManagedWorkspace interface {
	Workspace

	// ListMCPs returns all registered MCP server configurations.
	ListMCPs(ctx context.Context) ([]MCPInfo, error)

	// AddMCP registers an MCP server configuration by name.
	AddMCP(ctx context.Context, name string, config map[string]any) error

	// RemoveMCP removes a registered MCP server configuration.
	RemoveMCP(ctx context.Context, name string) error

	// ListSkills returns all registered skills.
	ListSkills(ctx context.Context) ([]SkillInfo, error)

	// AddSkill registers a skill from a path.
	AddSkill(ctx context.Context, path string) error

	// RemoveSkill removes a registered skill by name.
	RemoveSkill(ctx context.Context, name string) error

	// GetInstructions returns workspace-level instructions (e.g. from CLAUDE.md).
	GetInstructions(ctx context.Context) (string, error)
}

// EnhancedLocalWorkspace embeds a LocalWorkspace and adds MCP/skill
// management with file-based persistence.
type EnhancedLocalWorkspace struct {
	*LocalWorkspace
	mu sync.Mutex
}

// NewEnhancedLocalWorkspace creates a managed workspace rooted at the given path.
func NewEnhancedLocalWorkspace(cfg LocalConfig) (*EnhancedLocalWorkspace, error) {
	base, err := NewLocalWorkspace(cfg)
	if err != nil {
		return nil, err
	}
	return &EnhancedLocalWorkspace{LocalWorkspace: base}, nil
}

// mcpConfigFile is the name of the MCP configuration file in the workspace.
const mcpConfigFile = ".mcp.json"

// skillsDir is the subdirectory for skill files.
const skillsDir = "skills"

// skillsIndexFile is the name of the skills index file.
const skillsIndexFile = ".skills"

// ListMCPs returns all registered MCP server configurations from .mcp.json.
func (w *EnhancedLocalWorkspace) ListMCPs(_ context.Context) ([]MCPInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	configs, err := w.loadMCPConfigs()
	if err != nil {
		return nil, err
	}

	result := make([]MCPInfo, 0, len(configs))
	for name, cfg := range configs {
		cfgMap, ok := cfg.(map[string]any)
		if !ok {
			continue
		}
		result = append(result, MCPInfo{Name: name, Config: cfgMap})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result, nil
}

// AddMCP registers an MCP server configuration by name. The configuration
// is persisted to .mcp.json in the workspace root.
func (w *EnhancedLocalWorkspace) AddMCP(_ context.Context, name string, config map[string]any) error {
	if name == "" {
		return fmt.Errorf("workspace: MCP name is required")
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	configs, err := w.loadMCPConfigs()
	if err != nil {
		return err
	}

	configs[name] = config
	return w.saveMCPConfigs(configs)
}

// RemoveMCP removes a registered MCP server configuration.
func (w *EnhancedLocalWorkspace) RemoveMCP(_ context.Context, name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	configs, err := w.loadMCPConfigs()
	if err != nil {
		return err
	}

	if _, ok := configs[name]; !ok {
		return fmt.Errorf("workspace: MCP %q not found", name)
	}

	delete(configs, name)
	return w.saveMCPConfigs(configs)
}

// ListSkills returns all registered skills from the .skills index file.
func (w *EnhancedLocalWorkspace) ListSkills(_ context.Context) ([]SkillInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	skills, err := w.loadSkillsIndex()
	if err != nil {
		return nil, err
	}

	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Name < skills[j].Name
	})

	return skills, nil
}

// AddSkill registers a skill from a path. The skill name is derived from the
// base name of the path (without extension).
func (w *EnhancedLocalWorkspace) AddSkill(_ context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("workspace: skill path is required")
	}

	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if name == "" {
		return fmt.Errorf("workspace: could not derive skill name from path %q", path)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// Ensure skills directory exists
	dir := filepath.Join(w.basePath, skillsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("workspace: create skills dir: %w", err)
	}

	skills, err := w.loadSkillsIndex()
	if err != nil {
		return err
	}

	// Check for duplicate
	for _, s := range skills {
		if s.Name == name {
			return fmt.Errorf("workspace: skill %q already registered", name)
		}
	}

	skills = append(skills, SkillInfo{Name: name, Path: path})
	return w.saveSkillsIndex(skills)
}

// RemoveSkill removes a registered skill by name.
func (w *EnhancedLocalWorkspace) RemoveSkill(_ context.Context, name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	skills, err := w.loadSkillsIndex()
	if err != nil {
		return err
	}

	found := false
	filtered := skills[:0]
	for _, s := range skills {
		if s.Name == name {
			found = true
			continue
		}
		filtered = append(filtered, s)
	}

	if !found {
		return fmt.Errorf("workspace: skill %q not found", name)
	}

	return w.saveSkillsIndex(filtered)
}

// GetInstructions reads workspace-level instructions from CLAUDE.md or
// README.md in the workspace root.
func (w *EnhancedLocalWorkspace) GetInstructions(_ context.Context) (string, error) {
	for _, name := range []string{"CLAUDE.md", "README.md"} {
		path := filepath.Join(w.basePath, name)
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data), nil
		}
	}
	return "", nil // no instructions file is not an error
}

// loadMCPConfigs loads the MCP configuration from .mcp.json.
// Returns an empty map if the file does not exist.
func (w *EnhancedLocalWorkspace) loadMCPConfigs() (map[string]any, error) {
	path := filepath.Join(w.basePath, mcpConfigFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return make(map[string]any), nil
	}
	if err != nil {
		return nil, fmt.Errorf("workspace: read MCP config: %w", err)
	}

	var configs map[string]any
	if err := json.Unmarshal(data, &configs); err != nil {
		return nil, fmt.Errorf("workspace: parse MCP config: %w", err)
	}
	return configs, nil
}

// saveMCPConfigs writes the MCP configuration to .mcp.json.
func (w *EnhancedLocalWorkspace) saveMCPConfigs(configs map[string]any) error {
	data, err := json.MarshalIndent(configs, "", "  ")
	if err != nil {
		return fmt.Errorf("workspace: marshal MCP config: %w", err)
	}
	path := filepath.Join(w.basePath, mcpConfigFile)
	return fsutil.WriteFileAtomic(path, data, 0o644)
}

// loadSkillsIndex loads the skills index from .skills.
// Returns an empty slice if the file does not exist.
func (w *EnhancedLocalWorkspace) loadSkillsIndex() ([]SkillInfo, error) {
	path := filepath.Join(w.basePath, skillsIndexFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workspace: read skills index: %w", err)
	}

	var skills []SkillInfo
	if err := json.Unmarshal(data, &skills); err != nil {
		return nil, fmt.Errorf("workspace: parse skills index: %w", err)
	}
	return skills, nil
}

// saveSkillsIndex writes the skills index to .skills.
func (w *EnhancedLocalWorkspace) saveSkillsIndex(skills []SkillInfo) error {
	data, err := json.MarshalIndent(skills, "", "  ")
	if err != nil {
		return fmt.Errorf("workspace: marshal skills index: %w", err)
	}
	path := filepath.Join(w.basePath, skillsIndexFile)
	return fsutil.WriteFileAtomic(path, data, 0o644)
}

// Compile-time interface checks.
var _ ManagedWorkspace = (*EnhancedLocalWorkspace)(nil)
