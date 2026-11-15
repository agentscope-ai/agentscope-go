// Package evalkit is the task-based evaluation harness (HARNESS_DESIGN C1/C2):
// declarative task suites, a runner that executes them against any ChatModel
// with determinism controls, scorers (including trajectory and budget), and
// aggregate reports. Suites are YAML so they can live in the repo and grow
// into a regression corpus.
package evalkit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// TaskSpec is one evaluation task (suite YAML format).
type TaskSpec struct {
	ID      string   `yaml:"id"`
	Tags    []string `yaml:"tags,omitempty"`
	Input   string   `yaml:"input"`
	Turns   []string `yaml:"turns,omitempty"` // follow-up user turns (multi-talk tasks)
	System  string   `yaml:"system,omitempty"`
	Tools   []string `yaml:"tools,omitempty"`
	Fixture string   `yaml:"fixture,omitempty"` // directory copied into the task workspace

	Scorer ScorerSpec `yaml:"scorer"`

	Budget   BudgetSpec   `yaml:"budget,omitempty"`
	Sampling SamplingSpec `yaml:"sampling,omitempty"`
	Repeat   int          `yaml:"repeat,omitempty"` // 0/1 = single run
}

// ScorerSpec selects and configures the task scorer.
type ScorerSpec struct {
	Ref    string   `yaml:"ref"` // contains | json_field | text_contains | trajectory | budget
	Expect string   `yaml:"expect,omitempty"`
	Field  string   `yaml:"field,omitempty"`
	Items  []string `yaml:"items,omitempty"`  // trajectory: expected tool-call sequence
	Mode   string   `yaml:"mode,omitempty"`   // trajectory: exact | subsequence (default subsequence)
	Source string   `yaml:"source,omitempty"` // json_field: final | any
}

// BudgetSpec bounds one task run.
type BudgetSpec struct {
	MaxIters     int     `yaml:"max_iters,omitempty"`
	MaxCostUSD   float64 `yaml:"max_cost_usd,omitempty"`
	MaxInTokens  int     `yaml:"max_input_tokens,omitempty"`
	MaxOutTokens int     `yaml:"max_output_tokens,omitempty"`
}

// SamplingSpec pins sampling parameters for determinism (C5).
type SamplingSpec struct {
	Temperature *float64 `yaml:"temperature,omitempty"` // default: 0
	Seed        *int64   `yaml:"seed,omitempty"`        // provider seed where supported
}

// HasTag reports whether the task carries a tag.
func (t *TaskSpec) HasTag(tag string) bool {
	for _, tg := range t.Tags {
		if tg == tag {
			return true
		}
	}
	return false
}

// LoadSuiteDir loads every *.yaml/*.yml task under dir (recursive). Fixture
// paths are resolved relative to the file that declares them.
func LoadSuiteDir(dir string) ([]TaskSpec, error) {
	var tasks []TaskSpec
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var spec TaskSpec
		if err := yaml.Unmarshal(raw, &spec); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if spec.ID == "" {
			spec.ID = strings.TrimSuffix(filepath.Base(path), ext)
		}
		if spec.Fixture != "" && !filepath.IsAbs(spec.Fixture) {
			spec.Fixture = filepath.Join(filepath.Dir(path), spec.Fixture)
		}
		tasks = append(tasks, spec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tasks, nil
}

// --- Tool registry -------------------------------------------------------

// ToolFactory builds a fresh tool instance for a task run.
type ToolFactory func() tool.Tool

var (
	toolRegistryMu       sync.RWMutex
	builtinToolFactories = map[string]ToolFactory{}
)

// RegisterToolFactory adds (or overrides) a named tool factory available to
// task specs. Built-in file/shell tools are pre-registered. Safe for
// concurrent use (HARNESS review M6).
func RegisterToolFactory(name string, f ToolFactory) {
	toolRegistryMu.Lock()
	defer toolRegistryMu.Unlock()
	builtinToolFactories[name] = f
}

// LookupToolFactory returns the factory for name.
func LookupToolFactory(name string) (ToolFactory, bool) {
	toolRegistryMu.RLock()
	defer toolRegistryMu.RUnlock()
	f, ok := builtinToolFactories[name]
	return f, ok
}

func init() {
	// Pre-registered built-ins use the names the tools expose to models.
	RegisterToolFactory("view_text_file", tool.ViewTextFileTool)
	RegisterToolFactory("Read", tool.ReadTool)
	RegisterToolFactory("Edit", tool.EditTool)
	RegisterToolFactory("MultiEdit", tool.MultiEditTool)
	RegisterToolFactory("Glob", tool.GlobTool)
	RegisterToolFactory("Grep", tool.GrepTool)
	RegisterToolFactory("Bash", func() tool.Tool { return tool.BashTool() })
	RegisterToolFactory("execute_shell_command", tool.ExecuteShellCommandTool)
}
