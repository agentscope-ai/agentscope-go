package agent

import (
	"context"
	"fmt"
	"time"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// SpawnIsolation controls how isolated the subagent's execution environment is.
type SpawnIsolation int

const (
	SpawnIsolationNone     SpawnIsolation = iota // shared process
	SpawnIsolationWorktree                       // separate git worktree
)

// SpawnConfig configures a spawned subagent.
type SpawnConfig struct {
	Name         string
	SystemPrompt string
	Model        model.ChatModel
	Tools        []tool.Tool
	MaxIters     int
	Timeout      time.Duration
	Isolation    SpawnIsolation
}

// SpawnResult is the outcome of a subagent execution.
type SpawnResult struct {
	Output    string
	TokensIn  int
	TokensOut int
	Duration  time.Duration
	Events    []event.Event
}

// Spawn creates a temporary subagent, runs it with the given task, and returns
// the result. The subagent has its own state and toolkit but inherits the
// parent's model if none is specified.
func (a *UnifiedAgent) Spawn(ctx context.Context, cfg *SpawnConfig, task string) (*SpawnResult, error) {
	if cfg.Name == "" {
		cfg.Name = fmt.Sprintf("%s-sub-%s", a.name, agentscope.GenerateID()[:8])
	}

	m := cfg.Model
	if m == nil {
		m = a.model
	}

	maxIters := cfg.MaxIters
	if maxIters <= 0 {
		maxIters = defaultUnifiedMaxIters
	}

	prompt := cfg.SystemPrompt
	if prompt == "" {
		prompt = fmt.Sprintf("You are %s, a subagent spawned to handle a specific task. "+
			"Return your final answer as plain text. Be concise.", cfg.Name)
	}

	var opts []AgentOption
	if len(cfg.Tools) > 0 {
		tk := tool.NewToolkit(cfg.Tools...)
		opts = append(opts, WithToolkit(tk))
	}
	opts = append(opts, WithReactConfig(ReactConfig{MaxIters: maxIters}))

	sub := NewUnifiedAgent(cfg.Name, prompt, m, opts...)

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	subCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()

	ch, err := sub.ReplyStream(subCtx, task)
	if err != nil {
		return nil, fmt.Errorf("spawn %s: %w", cfg.Name, err)
	}

	result := &SpawnResult{}
	var lastText string
	for ev := range ch {
		result.Events = append(result.Events, ev)
		if e, ok := ev.(event.TextBlockDeltaEvent); ok {
			lastText += e.Delta
		}
	}

	result.Duration = time.Since(start)
	result.Output = lastText

	// Extract token usage from subagent state
	sub.mu.Lock()
	for _, msg := range sub.state.Context {
		if msg.Usage != nil {
			result.TokensIn += msg.Usage.InputTokens
			result.TokensOut += msg.Usage.OutputTokens
		}
	}
	sub.mu.Unlock()

	return result, nil
}
