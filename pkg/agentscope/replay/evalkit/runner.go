package evalkit

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// TaskResult is one task × repeat outcome.
type TaskResult struct {
	ErrorType    string        `json:"error_type,omitempty"`
	TaskID       string        `json:"task_id"`
	Repeat       int           `json:"repeat"`
	Pass         bool          `json:"pass"`
	Score        float64       `json:"score"`
	Iters        int           `json:"iters"`
	InputTokens  int           `json:"input_tokens"`
	OutputTokens int           `json:"output_tokens"`
	CostUSD      float64       `json:"cost_usd,omitempty"`
	Latency      time.Duration `json:"latency_ns"`
	Trajectory   []string      `json:"trajectory,omitempty"`
	Error        string        `json:"error,omitempty"`
	ScoreError   string        `json:"score_error,omitempty"`
}

// SuiteReport aggregates task results.
type SuiteReport struct {
	Suite     string        `json:"suite"`
	StartedAt time.Time     `json:"started_at"`
	Duration  time.Duration `json:"duration_ns"`
	Results   []TaskResult  `json:"results"`
}

// PassRate returns passing / total.
func (r *SuiteReport) PassRate() (int, int) {
	pass := 0
	for i := range r.Results {
		if r.Results[i].Pass {
			pass++
		}
	}
	return pass, len(r.Results)
}

// TotalTokens sums token usage across the suite.
func (r *SuiteReport) TotalTokens() (in, out int) {
	for i := range r.Results {
		in += r.Results[i].InputTokens
		out += r.Results[i].OutputTokens
	}
	return in, out
}

// Markdown renders a human-readable report.
func (r *SuiteReport) Markdown() string {
	var sb strings.Builder
	pass, total := r.PassRate()
	in, out := r.TotalTokens()
	fmt.Fprintf(&sb, "# Eval Suite Report: %s\n\n", r.Suite)
	fmt.Fprintf(&sb, "- Pass rate: **%d/%d** (%.0f%%)\n", pass, total, pct(pass, total))
	fmt.Fprintf(&sb, "- Tokens: %d input / %d output\n", in, out)
	fmt.Fprintf(&sb, "- Duration: %s\n\n", r.Duration.Round(time.Millisecond))
	sb.WriteString("| Task | Repeat | Score | Iters | In/Out tokens | Trajectory | Result |\n")
	sb.WriteString("|------|--------|-------|-------|---------------|------------|--------|\n")
	for i := range r.Results {
		res := &r.Results[i]
		status := "✅ pass"
		if !res.Pass {
			status = "❌ fail"
		}
		fmt.Fprintf(&sb, "| %s | %d | %.2f | %d | %d/%d | %s | %s |\n",
			res.TaskID, res.Repeat, res.Score, res.Iters,
			res.InputTokens, res.OutputTokens,
			strings.Join(res.Trajectory, "→"), status)
		if res.Error != "" {
			fmt.Fprintf(&sb, "| ↳ error | | | | | | %s |\n", truncStr(res.Error, 120))
		}
	}
	return sb.String()
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b) * 100
}

func truncStr(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Runner executes task suites.
type Runner struct {
	// WorkspaceRoot is where per-task workspaces are created (default:
	// os.TempDir()).
	WorkspaceRoot string
	// TaskTimeout bounds one task run (default: 5m).
	TaskTimeout time.Duration
	// DefaultMaxIters applies when a task declares no budget (default: 8).
	DefaultMaxIters int
	// DefaultSystem prompt when a task declares none.
	DefaultSystem string
	// ModelName enables cost accounting via model.ResolvePrice; leave empty
	// only when no task declares budget.max_cost_usd — the budget scorer
	// errors when a cost budget is declared but the model cannot be priced.
	ModelName string
}

func (r *Runner) withDefaults() Runner {
	out := *r
	if out.WorkspaceRoot == "" {
		out.WorkspaceRoot = os.TempDir()
	}
	if out.TaskTimeout <= 0 {
		out.TaskTimeout = 5 * time.Minute
	}
	if out.DefaultMaxIters <= 0 {
		out.DefaultMaxIters = 8
	}
	if out.DefaultSystem == "" {
		out.DefaultSystem = "You are a helpful assistant. Complete the task using the available tools."
	}
	return out
}

// RunSuite executes every task (× repeats) sequentially and aggregates.
func (r *Runner) RunSuite(ctx context.Context, suiteName string, tasks []TaskSpec, m model.ChatModel) (*SuiteReport, error) {
	rr := r.withDefaults()
	report := &SuiteReport{Suite: suiteName, StartedAt: time.Now()}
	for i := range tasks {
		task := &tasks[i]
		repeats := task.Repeat
		if repeats < 1 {
			repeats = 1
		}
		for i := 0; i < repeats; i++ {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			res := rr.RunTask(ctx, task, m)
			res.Repeat = i + 1
			report.Results = append(report.Results, res)
		}
	}
	report.Duration = time.Since(report.StartedAt)
	return report, nil
}

// RunTask executes one task once and scores it. It waits for the agent core and
// tool collectors before workspace cleanup; uncooperative models or tools can
// therefore delay its return after cancellation.
func (r *Runner) RunTask(ctx context.Context, task *TaskSpec, m model.ChatModel) TaskResult {
	execution := r.executeTask(ctx, task, m)
	defer execution.close()
	res := execution.result
	if res.Error != "" {
		return res
	}
	scorer, err := BuildScorer(&execution.task.Scorer)
	if err != nil {
		res.ScoreError = err.Error()
		return res
	}
	score, err := scorer.Score(ctx, &execution.task, &execution.outcome)
	if err != nil {
		res.ScoreError = err.Error()
		return res
	}
	res.Score, res.Pass = score, score >= 1.0
	return res
}

func (r *Runner) executeTask(ctx context.Context, task *TaskSpec, m model.ChatModel) *taskExecution {
	rr := r.withDefaults()
	execution := &taskExecution{task: cloneTask(task)}
	task = &execution.task
	res := &execution.result
	res.TaskID = task.ID
	defer func() {
		if execution.completedAt.IsZero() {
			execution.completedAt = time.Now()
		}
	}()

	workDir, err := os.MkdirTemp(rr.WorkspaceRoot, "evalkit-"+task.ID+"-")
	if err != nil {
		res.Error = fmt.Sprintf("workspace: %v", err)
		return execution
	}
	execution.workDir = workDir

	if task.Fixture != "" {
		if err := copyTree(task.Fixture, workDir); err != nil {
			res.Error = fmt.Sprintf("fixture: %v", err)
			return execution
		}
	}

	var taskTools []tool.Tool
	for _, name := range task.Tools {
		factory, ok := LookupToolFactory(name)
		if !ok {
			res.Error = fmt.Sprintf("unknown tool %q (register via evalkit.RegisterToolFactory)", name)
			return execution
		}
		taskTools = append(taskTools, factory())
	}
	tk := tool.NewToolkit(taskTools...)

	// Determinism (HARNESS_DESIGN C5): pin sampling; default temperature 0.
	temp := 0.0
	if task.Sampling.Temperature != nil {
		temp = *task.Sampling.Temperature
	}
	var seed *int64
	if task.Sampling.Seed != nil {
		v := *task.Sampling.Seed
		seed = &v
	}
	wrapped := &samplingModel{inner: m, temp: temp, seed: seed}

	maxIters := task.Budget.MaxIters
	if maxIters <= 0 {
		maxIters = rr.DefaultMaxIters
	}
	system := task.System
	if system == "" {
		system = rr.DefaultSystem
	}

	completion := &executionCompletion{}
	a := agent.NewUnifiedAgent("eval-"+task.ID, system, wrapped,
		agent.WithToolkit(tk),
		agent.WithLoopHooks(completion),
		agent.WithPermissionContext(permission.NewContext(permission.ModeBypass)),
		agent.WithReactConfig(agent.ReactConfig{MaxIters: maxIters}),
	)

	turns := append([]string{task.Input}, task.Turns...)
	for i := range turns {
		turns[i] = strings.ReplaceAll(turns[i], "{workspace}", workDir)
	}

	runCtx, cancel := context.WithTimeout(ctx, rr.TaskTimeout)
	defer cancel()

	start := time.Now()
	// Multi-turn: drive every turn against the SAME agent so the
	// conversation state accumulates; the outcome aggregates all replies.
	var out TaskOutcome
	for _, turn := range turns {
		completion.done = make(chan struct{})
		ch, err := a.ReplyStream(runCtx, turn)
		if err != nil {
			out.Error = fmt.Sprintf("reply: %v", err)
			break
		}
		step := collectOutcome(runCtx, ch)
		// The public event forwarder can close before a canceled tool exits.
		// Keep exclusive workspace ownership until the core has returned.
		<-completion.done
		if step.Error == "" && runCtx.Err() != nil {
			step.Error = runCtx.Err().Error()
		}
		out.FinalText = step.FinalText // last reply wins for scoring
		out.Trajectory = append(out.Trajectory, step.Trajectory...)
		out.Iters += step.Iters
		out.InputTokens += step.InputTokens
		out.OutputTokens += step.OutputTokens
		out.CacheCreateTokens += step.CacheCreateTokens
		out.CacheReadTokens += step.CacheReadTokens
		out.Events = append(out.Events, step.Events...)
		if step.Error != "" {
			out.Error = step.Error
			out.ErrorType = step.ErrorType
			break
		}
	}
	execution.completedAt = time.Now()
	res.Latency = execution.completedAt.Sub(start)
	res.Iters = out.Iters
	res.InputTokens = out.InputTokens
	res.OutputTokens = out.OutputTokens
	res.Trajectory = out.Trajectory
	res.Error = out.Error
	res.ErrorType = out.ErrorType

	// Cost accounting (HARNESS review M4): price the accumulated usage so
	// budget.max_cost_usd and reports carry real numbers.
	if rr.ModelName != "" {
		if price, ok := model.ResolvePrice(rr.ModelName); ok {
			out.CostUSD = price.CostUSD(&model.ChatUsage{
				InputTokens:              out.InputTokens,
				OutputTokens:             out.OutputTokens,
				CacheInputTokens:         out.CacheReadTokens,
				CacheCreationInputTokens: out.CacheCreateTokens,
			})
			out.CostPriced = true
			res.CostUSD = out.CostUSD
		}
	}

	out.Workspace = workDir
	execution.outcome = out
	return execution
}

// collectOutcome drains a reply stream into a TaskOutcome.
func collectOutcome(ctx context.Context, ch <-chan event.Event) TaskOutcome {
	var replyID string
	terminal := false
	pending := make(map[string]bool)
	var out TaskOutcome
	var text strings.Builder
	for evt := range ch {
		if terminal {
			continue
		}
		out.Events = append(out.Events, evt)
		switch e := evt.(type) {
		case event.ReplyStartEvent:
			replyID = e.ReplyID
		case event.ReplyEndEvent:
			terminal = true
			switch {
			case ctx.Err() != nil:
				out.Error = ctx.Err().Error()
			case replyID == "" || e.ReplyID != replyID:
				out.Error = "reply terminal does not match start"
			case e.Error != nil:
				out.Error = fmt.Sprintf("reply %s: %s", e.Error.Type, e.Error.Message)
				out.ErrorType = string(e.Error.Type)
			case e.FinishedReason != types.ReplyCompleted:
				out.Error = fmt.Sprintf("reply ended: %s", e.FinishedReason)
			case len(pending) != 0:
				out.Error = "reply ended with pending tools (interactive waits are not supported)"
			}
		case event.ToolResultEndEvent:
			delete(pending, e.ToolCallID)
		case event.ModelCallStartEvent:
			out.Iters++
		case event.ModelCallEndEvent:
			out.InputTokens += e.InputTokens
			out.OutputTokens += e.OutputTokens
			out.CacheCreateTokens += e.CacheCreationTokens
			out.CacheReadTokens += e.CacheReadTokens
		case event.ToolCallStartEvent:
			pending[e.ToolCallID] = true
			out.Trajectory = append(out.Trajectory, e.ToolCallName)
		case event.TextBlockDeltaEvent:
			text.WriteString(e.Delta)
		case event.CustomEvent:
			if strings.Contains(e.Name, "error") || e.Name == "model_partial_response" {
				out.Error = e.Name
			}
		}
	}
	if !terminal {
		if ctx.Err() != nil {
			out.Error = ctx.Err().Error()
		} else if out.Error == "" {
			out.Error = "reply stream closed without terminal event"
		}
	}
	out.FinalText = text.String()
	return out
}

// samplingModel pins sampling parameters on every call (C5).
type samplingModel struct {
	inner model.ChatModel
	temp  float64
	seed  *int64
}

func (s *samplingModel) callOpts(opts []model.CallOption) []model.CallOption {
	opts = append(opts, model.WithTemperature(s.temp))
	if s.seed != nil {
		opts = append(opts, model.WithSeed(*s.seed))
	}
	return opts
}

func (s *samplingModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	return s.inner.Chat(ctx, msgs, s.callOpts(opts)...)
}

func (s *samplingModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	return s.inner.ChatStream(ctx, msgs, s.callOpts(opts)...)
}

func (s *samplingModel) CountTokens(msgs []*message.Msg, tools []model.ToolSchema) int {
	return s.inner.CountTokens(msgs, tools)
}

// copyTree copies a fixture directory into dst (files only, recursive).
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		outF, err := os.Create(target)
		if err != nil {
			return err
		}
		defer func() { _ = outF.Close() }()
		_, err = io.Copy(outF, in)
		return err
	})
}
