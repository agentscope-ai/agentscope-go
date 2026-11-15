package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// AgentStatus represents the lifecycle state of a managed agent.
type AgentStatus int

const (
	AgentStatusRunning AgentStatus = iota
	AgentStatusDone
	AgentStatusFailed
	AgentStatusStopped
)

var agentStatusNames = [...]string{"running", "done", "failed", "stopped"}

func (s AgentStatus) String() string {
	if int(s) < len(agentStatusNames) {
		return agentStatusNames[s]
	}
	return "unknown"
}

// AgentConfig configures a spawned managed agent.
type AgentConfig struct {
	Name         string
	SystemPrompt string
	Model        model.ChatModel
	Tools        []tool.Tool
	MaxIters     int
	Timeout      time.Duration
	Background   bool
}

// ManagedAgent tracks the full lifecycle of a spawned subagent.
type ManagedAgent struct {
	ID         string
	Name       string
	Config     AgentConfig
	Status     AgentStatus
	StartedAt  time.Time
	FinishedAt time.Time
	Result     *agent.SpawnResult
	Err        error

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// Wait blocks until the agent finishes or ctx is canceled.
func (ma *ManagedAgent) Wait(ctx context.Context) error {
	select {
	case <-ma.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ManagedAgentInfo is a read-only snapshot of a ManagedAgent's state.
type ManagedAgentInfo struct {
	ID         string
	Name       string
	Status     AgentStatus
	StartedAt  time.Time
	FinishedAt time.Time
	Result     *agent.SpawnResult
	Err        error
}

// Info returns a thread-safe snapshot of the agent's current state.
func (ma *ManagedAgent) Info() ManagedAgentInfo {
	ma.mu.Lock()
	defer ma.mu.Unlock()
	return ManagedAgentInfo{
		ID:         ma.ID,
		Name:       ma.Name,
		Status:     ma.Status,
		StartedAt:  ma.StartedAt,
		FinishedAt: ma.FinishedAt,
		Result:     ma.Result,
		Err:        ma.Err,
	}
}

// AgentManager manages the lifecycle of spawned subagents, integrating with
// BudgetTracker for concurrency limits and SessionHookManager for lifecycle
// notifications.
type AgentManager struct {
	mu     sync.RWMutex
	agents map[string]*ManagedAgent
	budget *BudgetTracker
	hooks  *SessionHookManager
}

// NewAgentManager creates a new AgentManager. Both budget and hooks may be nil
// if those features are not needed.
func NewAgentManager(budget *BudgetTracker, hooks *SessionHookManager) *AgentManager {
	return &AgentManager{
		agents: make(map[string]*ManagedAgent),
		budget: budget,
		hooks:  hooks,
	}
}

// Spawn creates and runs a subagent. If cfg.Background is true, the agent runs
// asynchronously and Spawn returns immediately. Otherwise, Spawn blocks until
// the agent completes.
//
// parentModel is used as the model when cfg.Model is nil.
func (am *AgentManager) Spawn(ctx context.Context, parentModel model.ChatModel, cfg *AgentConfig, task string) (*ManagedAgent, error) {
	if am.budget != nil {
		if err := am.budget.AcquireAgent(); err != nil {
			return nil, fmt.Errorf("agent budget exceeded: %w", err)
		}
	}

	id := agentscope.GenerateID()
	name := cfg.Name
	if name == "" {
		name = "agent-" + id[:8]
	}

	ma := &ManagedAgent{
		ID:        id,
		Name:      name,
		Config:    *cfg,
		Status:    AgentStatusRunning,
		StartedAt: time.Now(),
		done:      make(chan struct{}),
	}

	am.mu.Lock()
	am.agents[id] = ma
	am.mu.Unlock()

	if am.hooks != nil {
		_ = am.hooks.Fire(HookSubagentStart, map[string]any{
			"agent_id":   id,
			"agent_name": name,
			"task":       task,
		})
	}

	m := cfg.Model
	if m == nil {
		m = parentModel
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	subCtx, cancel := context.WithTimeout(ctx, timeout)
	ma.mu.Lock()
	ma.cancel = cancel
	ma.mu.Unlock()

	if cfg.Background {
		go am.executeAgent(subCtx, cancel, ma, m, cfg, task)
		return ma, nil
	}

	am.executeAgent(subCtx, cancel, ma, m, cfg, task)
	return ma, nil
}

func (am *AgentManager) executeAgent(subCtx context.Context, cancel context.CancelFunc, ma *ManagedAgent, m model.ChatModel, cfg *AgentConfig, task string) {
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			ma.mu.Lock()
			ma.Status = AgentStatusFailed
			ma.Err = fmt.Errorf("panic: %v", r)
			ma.FinishedAt = time.Now()
			ma.mu.Unlock()
		}

		if am.budget != nil {
			am.budget.ReleaseAgent()
		}
		if am.hooks != nil {
			ma.mu.Lock()
			status := ma.Status
			ma.mu.Unlock()
			_ = am.hooks.Fire(HookSubagentEnd, map[string]any{
				"agent_id":   ma.ID,
				"agent_name": ma.Name,
				"status":     status.String(),
			})
		}
		close(ma.done)
	}()

	maxIters := cfg.MaxIters
	if maxIters <= 0 {
		maxIters = 20
	}

	prompt := cfg.SystemPrompt
	if prompt == "" {
		prompt = fmt.Sprintf("You are %s, a subagent spawned to handle a specific task. "+
			"Return your final answer as plain text. Be concise.", ma.Name)
	}

	var opts []agent.AgentOption
	if len(cfg.Tools) > 0 {
		tk := tool.NewToolkit(cfg.Tools...)
		opts = append(opts, agent.WithToolkit(tk))
	}
	opts = append(opts, agent.WithReactConfig(agent.ReactConfig{MaxIters: maxIters}))

	sub := agent.NewUnifiedAgent(ma.Name, prompt, m, opts...)

	start := time.Now()
	ch, err := sub.ReplyStream(subCtx, task)
	if err != nil {
		ma.mu.Lock()
		ma.Status = AgentStatusFailed
		ma.Err = fmt.Errorf("spawn %s: %w", ma.Name, err)
		ma.FinishedAt = time.Now()
		ma.mu.Unlock()
		return
	}

	result := &agent.SpawnResult{}
	var lastText string
	var totalTokens int
	for ev := range ch {
		result.Events = append(result.Events, ev)
		if e, ok := ev.(event.TextBlockDeltaEvent); ok {
			lastText += e.Delta
		}
		if e, ok := ev.(event.ModelCallEndEvent); ok {
			result.TokensIn += e.InputTokens
			result.TokensOut += e.OutputTokens
			totalTokens += e.InputTokens + e.OutputTokens
		}
	}
	result.Duration = time.Since(start)
	result.Output = lastText

	if am.budget != nil && totalTokens > 0 {
		_ = am.budget.AddTokens(totalTokens)
	}

	ma.mu.Lock()
	if subCtx.Err() != nil {
		ma.Status = AgentStatusStopped
	} else {
		ma.Status = AgentStatusDone
	}
	ma.Result = result
	ma.FinishedAt = time.Now()
	ma.mu.Unlock()
}

// Stop cancels a running agent and waits up to 5 seconds for it to finish.
func (am *AgentManager) Stop(id string) error {
	am.mu.RLock()
	ma, ok := am.agents[id]
	am.mu.RUnlock()
	if !ok {
		return fmt.Errorf("agent %s not found", id)
	}

	ma.mu.Lock()
	if ma.Status != AgentStatusRunning {
		ma.mu.Unlock()
		return nil
	}
	cancel := ma.cancel
	ma.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	ctx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWait()
	return ma.Wait(ctx)
}

// Get returns the live managed-agent handle with the given ID, for control
// operations (Wait, etc.). Do NOT read its mutable fields (Status/Result/Err/
// FinishedAt) directly — those are written by the background goroutine; use
// ManagedAgent.Info() for a race-free snapshot (as List does).
func (am *AgentManager) Get(id string) (*ManagedAgent, bool) {
	am.mu.RLock()
	defer am.mu.RUnlock()
	ma, ok := am.agents[id]
	return ma, ok
}

// List returns a snapshot of all managed agents.
func (am *AgentManager) List() []*ManagedAgentInfo {
	am.mu.RLock()
	defer am.mu.RUnlock()
	infos := make([]*ManagedAgentInfo, 0, len(am.agents))
	for _, ma := range am.agents {
		info := ma.Info()
		infos = append(infos, &info)
	}
	return infos
}

// ActiveCount returns the number of agents currently running.
func (am *AgentManager) ActiveCount() int {
	am.mu.RLock()
	defer am.mu.RUnlock()
	count := 0
	for _, ma := range am.agents {
		ma.mu.Lock()
		if ma.Status == AgentStatusRunning {
			count++
		}
		ma.mu.Unlock()
	}
	return count
}

// WaitAll blocks until all managed agents have finished or ctx is canceled.
func (am *AgentManager) WaitAll(ctx context.Context) {
	am.mu.RLock()
	agents := make([]*ManagedAgent, 0, len(am.agents))
	for _, ma := range am.agents {
		agents = append(agents, ma)
	}
	am.mu.RUnlock()

	for _, ma := range agents {
		select {
		case <-ma.done:
		case <-ctx.Done():
			return
		}
	}
}

// StopAll cancels all running agents.
func (am *AgentManager) StopAll() {
	am.mu.RLock()
	ids := make([]string, 0, len(am.agents))
	for id, ma := range am.agents {
		ma.mu.Lock()
		running := ma.Status == AgentStatusRunning
		ma.mu.Unlock()
		if running {
			ids = append(ids, id)
		}
	}
	am.mu.RUnlock()

	for _, id := range ids {
		_ = am.Stop(id)
	}
}
