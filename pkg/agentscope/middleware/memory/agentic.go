package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
)

const (
	// FilenameMemoryMD is the index file of the agentic memory store.
	FilenameMemoryMD = "MEMORY.md"
	// DefaultAgenticMemoryDir is the workspace-relative memory directory.
	DefaultAgenticMemoryDir = "Memory"

	defaultAgenticMemoryMaxTokens = 4000
	defaultAgenticMiddlewareKey   = "agentic_memory"
)

// AgenticMemoryConfig configures the AgenticMemoryMiddleware.
type AgenticMemoryConfig struct {
	Workdir       string // required: agent workspace directory holding the memory dir
	MemoryDir     string // default "Memory"
	MaxTokens     int    // default 4000 — cap for the MEMORY.md snapshot in the prompt
	Instructions  string // default DefaultAgenticMemoryInstructions ({memory_dir} placeholder)
	MiddlewareKey string // default "agentic_memory"
}

func (c *AgenticMemoryConfig) applyDefaults() {
	if c.MemoryDir == "" {
		c.MemoryDir = DefaultAgenticMemoryDir
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = defaultAgenticMemoryMaxTokens
	}
	if c.Instructions == "" {
		c.Instructions = DefaultAgenticMemoryInstructions
	}
	if c.MiddlewareKey == "" {
		c.MiddlewareKey = defaultAgenticMiddlewareKey
	}
}

// AgenticMemoryMiddleware is the file-backed long-term memory where the LLM
// decides when and what to save (port of Python's AgenticMemoryMiddleware,
// #2263): the middleware maintains a workspace-local Markdown memory store,
// injects the memory instructions plus a bounded MEMORY.md snapshot into
// the system prompt, and the agent maintains the store with its regular
// file tools (Write/Read).
//
// Deliberate delta from Python: the asynchronous LLM-driven relevance
// retrieval (on_reply/on_reasoning hint blocks) is not ported yet — the
// selection text is kept as DefaultAgenticRetrievalInstructions for the
// future port. Until then the agent locates topic files itself via the
// memory instructions.
type AgenticMemoryMiddleware struct {
	middleware.BaseMiddleware
	cfg AgenticMemoryConfig
}

// Compile-time interface check.
var _ middleware.Middleware = (*AgenticMemoryMiddleware)(nil)

// NewAgenticMemory creates the middleware. Workdir must be set.
func NewAgenticMemory(cfg AgenticMemoryConfig) (*AgenticMemoryMiddleware, error) {
	if cfg.Workdir == "" {
		return nil, fmt.Errorf("memory: agentic memory requires a workdir")
	}
	cfg.applyDefaults()
	return &AgenticMemoryMiddleware{
		BaseMiddleware: middleware.BaseMiddleware{MiddlewareKey: cfg.MiddlewareKey},
		cfg:            cfg,
	}, nil
}

// MemoryDirPath returns the absolute memory directory.
func (m *AgenticMemoryMiddleware) MemoryDirPath() string {
	return filepath.Join(m.cfg.Workdir, m.cfg.MemoryDir)
}

// MemoryMDPath returns the absolute MEMORY.md path.
func (m *AgenticMemoryMiddleware) MemoryMDPath() string {
	return filepath.Join(m.MemoryDirPath(), FilenameMemoryMD)
}

// ensureLayout creates the memory directory and an empty MEMORY.md when
// missing (O_EXCL keeps concurrent first-touches honest).
func (m *AgenticMemoryMiddleware) ensureLayout() error {
	if err := os.MkdirAll(m.MemoryDirPath(), 0o755); err != nil {
		return fmt.Errorf("memory: create memory dir: %w", err)
	}
	f, err := os.OpenFile(m.MemoryMDPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("memory: create MEMORY.md: %w", err)
	}
	return f.Close()
}

// OnSystemPrompt appends the memory instructions and a bounded MEMORY.md
// snapshot to the system prompt.
func (m *AgenticMemoryMiddleware) OnSystemPrompt(_ context.Context, _ string, currentPrompt string) string {
	if err := m.ensureLayout(); err != nil {
		// A broken layout must not break the reply; the prompt simply
		// misses the snapshot this round.
		return currentPrompt
	}
	content, err := os.ReadFile(m.MemoryMDPath())
	if err != nil {
		return currentPrompt
	}
	snapshot := truncateToTokens(string(content), m.cfg.MaxTokens)

	if len(snapshot) != len(content) {
		remainLines := strings.Count(snapshot, "\n") + 1
		omittedLines := strings.Count(string(content), "\n") - strings.Count(snapshot, "\n")
		snapshot += "\n<<<TRUNCATED>>>\n<system-reminder>The remaining " +
			fmt.Sprintf("%d", omittedLines) + " lines have been omitted due to context " +
			"length limits. Use the `Read` tool with offset " +
			fmt.Sprintf("`%d`", remainLines) + " to access the rest of '" + m.MemoryMDPath() + "'." +
			"</system-reminder>"
	}
	if snapshot == "" {
		snapshot = "Your MEMORY.md is currently empty. When you save new " +
			"memories, they will appear here."
	}

	instructions := strings.ReplaceAll(m.cfg.Instructions, "{memory_dir}", m.MemoryDirPath())
	return currentPrompt + "\n\n" + instructions + "\n## MEMORY.md\n" + snapshot
}

// estimateTokens mirrors Python's _estimate_tokens (UTF-8 bytes / 4).
func estimateTokens(text string) int {
	return int(float64(len([]byte(text)))/4 + 0.5)
}

// truncateToTokens returns content capped at maxTokens estimated tokens
// (port of Python's _truncate_if_needed), never splitting a rune.
func truncateToTokens(content string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	if n := estimateTokens(content); n <= maxTokens {
		return content
	}
	index := int(float64(maxTokens) / float64(estimateTokens(content)) * float64(len(content)))
	for index > 0 && estimateTokens(content[:index]) > maxTokens {
		index -= 10
		if index < 0 {
			index = 0
		}
	}
	for index > 0 && index < len(content) && !utf8.RuneStart(content[index]) {
		index--
	}
	return content[:index]
}
