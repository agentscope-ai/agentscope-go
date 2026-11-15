package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// Mode controls how the memory middleware operates.
type Mode string

const (
	// ModeStaticControl automatically searches and injects memories into the
	// system prompt, and writes back conversation content after each reply.
	// The agent has no direct tool access to memories.
	ModeStaticControl Mode = "static_control"

	// ModeAgentControl provides search_memory and add_memory tools to the
	// agent, and appends usage instructions to the system prompt. No automatic
	// search or write-back.
	ModeAgentControl Mode = "agent_control"

	// ModeBoth combines static_control and agent_control: automatic memory
	// injection plus tool access.
	ModeBoth Mode = "both"
)

const (
	defaultTopK          = 5
	defaultMiddlewareKey = "longterm_memory"
	mcFieldMemories      = "memories"
	mcFieldAssistantText = "assistant_text"

	DefaultMemorySectionHeader = "## Relevant memories from past conversations"
	DefaultMemorySectionIntro  = "The following memories about the user may be relevant. " +
		"Use them only if they are pertinent to the current request."
	DefaultToolInstructions = "## Long-term memory\n\n" +
		"You have `search_memory` and `add_memory` tools available. Use them " +
		"whenever the conversation depends on (search) or contributes (add) a " +
		"durable fact about the user -- see each tool's own description for " +
		"the exact input shape and usage guidance."
)

// Config configures the LongTermMemoryMiddleware.
type Config struct {
	UserID  string      // required
	AgentID string      // optional; scopes memory per agent
	Store   MemoryStore // required

	Mode                Mode   // default: ModeBoth
	TopK                int    // default: 5
	ScopeSearchByAgent  bool   // filter search results by AgentID (default: false)
	MemorySectionHeader string // default: DefaultMemorySectionHeader
	MemorySectionIntro  string // default: DefaultMemorySectionIntro
	ToolInstructions    string // default: DefaultToolInstructions
}

func (c *Config) applyDefaults() {
	if c.Mode == "" {
		c.Mode = ModeBoth
	}
	if c.TopK <= 0 {
		c.TopK = defaultTopK
	}
	if c.MemorySectionHeader == "" {
		c.MemorySectionHeader = DefaultMemorySectionHeader
	}
	if c.MemorySectionIntro == "" {
		c.MemorySectionIntro = DefaultMemorySectionIntro
	}
	if c.ToolInstructions == "" {
		c.ToolInstructions = DefaultToolInstructions
	}
}

// LongTermMemoryMiddleware provides cross-session memory for agents using a
// MemoryStore backend. It supports three modes: static_control (automatic
// search/inject), agent_control (tools only), and both.
type LongTermMemoryMiddleware struct {
	middleware.BaseMiddleware
	cfg   Config
	tools []tool.Tool
}

// New creates a LongTermMemoryMiddleware with the given configuration.
func New(cfg *Config) (*LongTermMemoryMiddleware, error) {
	if cfg.UserID == "" {
		return nil, fmt.Errorf("memory middleware: user_id is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("memory middleware: store is required")
	}
	cfg.applyDefaults()

	m := &LongTermMemoryMiddleware{
		BaseMiddleware: middleware.BaseMiddleware{MiddlewareKey: defaultMiddlewareKey},
		cfg:            *cfg,
	}

	if cfg.Mode == ModeAgentControl || cfg.Mode == ModeBoth {
		m.tools = NewMemoryTools(cfg.Store, cfg.UserID, cfg.AgentID, cfg.ScopeSearchByAgent)
	}

	return m, nil
}

// Tools returns the memory tools (search_memory, add_memory) provided by this
// middleware. Add these to the agent's toolkit for agent_control or both modes.
// Returns nil for static_control mode.
func (m *LongTermMemoryMiddleware) Tools() []tool.Tool {
	return m.tools
}

// OnReply wraps the reply lifecycle. In static_control/both modes, it searches
// for relevant memories before the reply and writes back the conversation after.
func (m *LongTermMemoryMiddleware) OnReply(ctx context.Context, input middleware.ReplyInput, next middleware.ReplyHandler) <-chan event.Event {
	if m.cfg.Mode == ModeAgentControl {
		return next(ctx, input)
	}

	query := input.UserInput
	if query != "" {
		opts := &SearchOptions{TopK: m.cfg.TopK}
		if m.cfg.ScopeSearchByAgent && m.cfg.AgentID != "" {
			opts.AgentID = m.cfg.AgentID
		}
		memories, err := m.cfg.Store.Search(ctx, query, m.cfg.UserID, opts)
		if err == nil && len(memories) > 0 {
			if mc := middleware.GetMiddleContext(ctx); mc != nil {
				mc.Set(m.Key(), mcFieldMemories, memories)
			}
		}
	}

	innerCh := next(ctx, input)
	outCh := make(chan event.Event, 16)

	go func() {
		defer close(outCh)
		var textParts []string

		for ev := range innerCh {
			outCh <- ev
			if delta, ok := ev.(event.TextBlockDeltaEvent); ok {
				textParts = append(textParts, delta.Delta)
			}
		}

		assistantText := strings.Join(textParts, "")
		if query != "" && assistantText != "" {
			writeText := fmt.Sprintf("User: %s\nAssistant: %s", query, assistantText)
			_ = m.cfg.Store.Add(ctx, writeText, m.cfg.UserID, m.cfg.AgentID)
		}
	}()

	return outCh
}

// OnSystemPrompt injects cached memories and/or tool instructions into the
// system prompt.
func (m *LongTermMemoryMiddleware) OnSystemPrompt(ctx context.Context, _ string, currentPrompt string) string {
	prompt := currentPrompt

	if m.cfg.Mode != ModeAgentControl {
		if mc := middleware.GetMiddleContext(ctx); mc != nil {
			if v, ok := mc.Get(m.Key(), mcFieldMemories); ok {
				if memories, ok := v.([]Memory); ok && len(memories) > 0 {
					var sb strings.Builder
					sb.WriteString("\n\n")
					sb.WriteString(m.cfg.MemorySectionHeader)
					sb.WriteString("\n")
					sb.WriteString(m.cfg.MemorySectionIntro)
					sb.WriteString("\n")
					for _, mem := range memories {
						sb.WriteString("- ")
						sb.WriteString(mem.Text)
						sb.WriteString("\n")
					}
					prompt += sb.String()
				}
			}
		}
	}

	if m.cfg.Mode == ModeAgentControl || m.cfg.Mode == ModeBoth {
		prompt += "\n\n" + m.cfg.ToolInstructions
	}

	return prompt
}
