package evalkit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/bench"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// ErrAdmissionRejected identifies an application rejection returned by NewModel.
// It is distinct from the load generator's in-flight limit and provider failures.
var ErrAdmissionRejected = errors.New("evalkit: application admission rejected")

// TaskArrival assigns a task and one-based repeat to a scheduled arrival.
type TaskArrival struct {
	TaskID string        `json:"task_id"`
	Repeat int           `json:"repeat"`
	Offset time.Duration `json:"offset_ns"`
}

// WorkloadManifest pins workload identity and acceptance criteria. Version must
// be 1. Metadata should record model/server/tokenizer revisions, actual context
// window, hardware/quotas, prompts, cache/retry settings and experiment group.
// RunID must be unique per experiment; iteration is the one-based arrival index.
// Callers must not mutate a manifest while RunLoad copies it.
type WorkloadManifest struct {
	Version          int               `json:"version"`
	RunID            string            `json:"run_id"`
	Scenario         string            `json:"scenario"`
	TaskSetVersion   string            `json:"task_set_version"`
	SourceRevision   string            `json:"source_revision"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	QualityThreshold float64           `json:"quality_threshold"`
	Tasks            []TaskSpec        `json:"tasks"`
	Arrivals         []TaskArrival     `json:"arrivals"`
}

// LoadConfig bounds execution, retained workspaces, and the separate score phase.
// NewModel is called once per entered task; it must return a fresh model or a
// model safe for the configured concurrency. Scorer, when set, must likewise be
// safe for MaxScorers concurrent calls; otherwise each task builds its own scorer.
// Factories, models, tools and scorers must honor cancellation. Uncooperative
// execution/scoring can outlive observation, retaining at most MaxInFlight /
// MaxScorers workers and their workspaces. Terminate them before repeated runs.
// Tools must stop accessing their workspace before Execute returns (or their
// stream closes). Detached work is the host's responsibility.
type LoadConfig struct {
	AttemptLedger     *inference.Ledger // Optional physical facts; use the same source as managed deployments.
	MaxInFlight       int
	TaskTimeout       time.Duration // Scheduled-arrival deadline, including dispatch lag.
	DrainTimeout      time.Duration
	MaxPendingScores  int // Positive bound on both task definitions and planned arrivals.
	MaxScorers        int
	ScoreTimeout      time.Duration
	ScorePhaseTimeout time.Duration
	NewModel          func(context.Context, TaskSpec) (model.ChatModel, error)
	Scorer            Scorer
}

// QualityStatus separates execution and scoring outcomes.
type QualityStatus string

const (
	QualityNotExecuted         QualityStatus = "not_executed"
	QualityExecutionFailed     QualityStatus = "execution_failed"
	QualityExecutionUnfinished QualityStatus = "execution_unfinished"
	QualityUnscored            QualityStatus = "unscored"
	QualityScoreUnfinished     QualityStatus = "score_unfinished"
	QualityScoreError          QualityStatus = "score_error"
	QualityScored              QualityStatus = "scored"
)

// TaskQualityResult joins an immutable arrival observation to its task result.
// CallbackEnteredAt is actual observed callback entry; Arrival.StartedAt remains
// generator authorization. A successful callback alone does not establish quality.
type TaskQualityResult struct {
	InferenceAttemptIDs []uint64             `json:"inference_attempt_ids,omitempty"`
	InferenceUnmanaged  bool                 `json:"inference_unmanaged,omitempty"`
	Iteration           int                  `json:"iteration"`
	Arrival             bench.OpenLoopResult `json:"arrival"`
	CallbackEnteredAt   time.Time            `json:"callback_entered_at,omitempty"`
	ExecutionFinishedAt time.Time            `json:"execution_finished_at,omitempty"`
	Task                TaskResult           `json:"task"`
	QualityStatus       QualityStatus        `json:"quality_status"`
	ScoreStartedAt      time.Time            `json:"score_started_at,omitempty"`
	ScoreFinishedAt     time.Time            `json:"score_finished_at,omitempty"`
}

// LoadLimits records the effective local limits used by RunLoad.
type LoadLimits struct {
	MaxInFlight       int           `json:"max_in_flight"`
	TaskTimeout       time.Duration `json:"task_timeout_ns"`
	ExecutionTimeout  time.Duration `json:"execution_timeout_ns"`
	DrainTimeout      time.Duration `json:"drain_timeout_ns"`
	MaxPendingScores  int           `json:"max_pending_scores"`
	MaxScorers        int           `json:"max_scorers"`
	ScoreTimeout      time.Duration `json:"score_timeout_ns"`
	ScorePhaseTimeout time.Duration `json:"score_phase_timeout_ns"`
}

// LoadReport is versioned, detached from late execution/scoring, and includes
// every planned arrival. Goodput counts quality-passing tasks completed before
// their scheduled deadline per observed execution second; nil means no interval.
// Latencies uses accepted completions only; CompletionSamples states its size.
// Optional Inference records managed physical attempts at a separate final cutoff.
// Its IDs join to rows; logical TaskResult cost estimates must not be added to it.
type LoadReport struct {
	Inference          *inference.Snapshot   `json:"inference,omitempty"`
	InferenceUnmatched int                   `json:"inference_unmatched,omitempty"`
	Limits             LoadLimits            `json:"limits"`
	Version            int                   `json:"version"`
	Manifest           WorkloadManifest      `json:"manifest"`
	Execution          *bench.OpenLoopReport `json:"execution"`
	ScoringStartedAt   time.Time             `json:"scoring_started_at"`
	ScoringFinishedAt  time.Time             `json:"scoring_finished_at"`
	Planned            int                   `json:"planned"`
	Offered            int                   `json:"offered"`
	Authorized         int                   `json:"authorized"`
	CallbackEntered    int                   `json:"callback_entered"`
	CompletionSamples  int                   `json:"completion_samples"`
	Latencies          *bench.LatencyStats   `json:"completion_latencies,omitempty"`
	Goodput            *float64              `json:"quality_goodput,omitempty"`
	Results            []TaskQualityResult   `json:"results"`
}

// RunLoad executes scheduled tasks, freezes their observed outcomes, then scores
// accepted successes under scoreCtx, independently of execution callback contexts.
// A caller interruption returns its partial report and error; score cancellation
// is recorded in quality statuses. No task is rerun for scoring. Workspaces stay
// owned by execution until the agent core and tool collectors return, then by a
// scorer until it returns. Late workers cannot modify the returned report.
func (r *Runner) RunLoad(execCtx, scoreCtx context.Context, manifest *WorkloadManifest, cfg *LoadConfig) (*LoadReport, error) {
	m, tasks, err := validateWorkload(execCtx, scoreCtx, manifest, cfg)
	if err != nil {
		return nil, err
	}
	config := *cfg
	cfg = &config
	rr := r.withDefaults()
	// The outer scheduled deadline already bounds execution; retain the runner's
	// independent task bound as well.
	state := &loadExecutionState{unmanaged: make([]bool, len(m.Arrivals)), open: true, candidates: make([]*taskExecution, len(m.Arrivals)), entered: make([]time.Time, len(m.Arrivals))}
	offsets := make([]time.Duration, len(m.Arrivals))
	for j, a := range m.Arrivals {
		offsets[j] = a.Offset
	}
	scenario := &bench.OpenLoopScenario{
		Name:           m.Scenario,
		ArrivalOffsets: offsets,
		MaxInFlight:    cfg.MaxInFlight,
		Timeout:        cfg.TaskTimeout,
		DrainTimeout:   cfg.DrainTimeout,
	}
	scenario.Run = func(ctx context.Context, iteration int) error {
		index := iteration - 1
		state.mu.Lock()
		if !state.open {
			state.mu.Unlock()
			return context.Canceled
		}
		state.entered[index] = time.Now()
		state.unmanaged[index] = cfg.AttemptLedger != nil
		state.mu.Unlock()
		arrival := m.Arrivals[index]
		ctx = inference.WithAttribution(ctx, inference.Attribution{RunID: m.RunID, Scenario: m.Scenario, Iteration: iteration, TaskID: arrival.TaskID, Repeat: arrival.Repeat})
		task := tasks[arrival.TaskID]
		task = cloneTask(&task)
		var e *taskExecution
		var factoryContextError error
		cm, err := cfg.NewModel(ctx, cloneTask(&task))
		if err == nil && cm == nil {
			err = errors.New("model factory returned nil")
		}
		if err != nil {
			// A failing factory may already have performed opaque warmup calls.
			// Without a bound model, its accounting remains unverified.
			kind := "model_setup"
			if errors.Is(err, context.DeadlineExceeded) {
				factoryContextError = context.DeadlineExceeded
			} else if errors.Is(err, context.Canceled) {
				factoryContextError = context.Canceled
			}
			if errors.Is(err, ErrAdmissionRejected) {
				kind = "admission_rejected"
			}
			e = &taskExecution{task: task, result: TaskResult{TaskID: task.ID, Error: errorDetail(err, "model factory failed without a message"), ErrorType: kind}, completedAt: time.Now()}
		} else {
			if cfg.AttemptLedger != nil {
				managed, ok := cm.(model.ManagedChatModel)
				covered := ok && managed.ManagedDeployment() != nil && managed.ManagedDeployment().Ledger() == cfg.AttemptLedger
				state.mu.Lock()
				if state.open {
					state.unmanaged[index] = !covered
				}
				state.mu.Unlock()
			}
			e = rr.executeTask(ctx, &task, cm)
		}
		e.result.Repeat = arrival.Repeat
		var callbackErr error
		if e.result.Error != "" {
			if factoryContextError != nil {
				callbackErr = fmt.Errorf("%s: %w", e.result.Error, factoryContextError)
			} else {
				callbackErr = errors.New(e.result.Error)
			}
		}
		state.mu.Lock()
		accepted := state.open
		if accepted {
			state.candidates[index] = e
		}
		state.mu.Unlock()
		if !accepted {
			e.close()
		}
		return callbackErr
	}
	execution, runErr := bench.NewRunner().RunOpenLoop(execCtx, scenario)
	if execution == nil {
		return nil, runErr
	}
	state.mu.Lock()
	state.open = false
	candidates, entered := state.candidates, state.entered
	unmanaged := append([]bool(nil), state.unmanaged...)
	state.candidates, state.entered = nil, nil
	state.mu.Unlock()
	report := &LoadReport{
		Limits: LoadLimits{
			MaxInFlight:       cfg.MaxInFlight,
			TaskTimeout:       cfg.TaskTimeout,
			ExecutionTimeout:  rr.TaskTimeout,
			DrainTimeout:      cfg.DrainTimeout,
			MaxPendingScores:  cfg.MaxPendingScores,
			MaxScorers:        cfg.MaxScorers,
			ScoreTimeout:      cfg.ScoreTimeout,
			ScorePhaseTimeout: cfg.ScorePhaseTimeout,
		},
		Version:   1,
		Manifest:  cloneWorkload(&m),
		Execution: execution,
		Planned:   len(m.Arrivals),
		Offered:   execution.Offered,
		Results:   make([]TaskQualityResult, len(m.Arrivals)),
	}
	pending := joinLoadExecution(report, candidates, entered)
	scoreLoad(scoreCtx, cfg, report, pending)
	summarizeLoad(report, cfg.TaskTimeout)
	joinInference(report, cfg.AttemptLedger, unmanaged)
	return report, runErr
}

type loadExecutionState struct {
	unmanaged  []bool
	mu         sync.Mutex
	open       bool
	candidates []*taskExecution
	entered    []time.Time
}

func validateWorkload(execCtx, scoreCtx context.Context, m *WorkloadManifest, cfg *LoadConfig) (WorkloadManifest, map[string]TaskSpec, error) {
	fail := func(detail string) (WorkloadManifest, map[string]TaskSpec, error) {
		return WorkloadManifest{}, nil, fmt.Errorf("evalkit: %s", detail)
	}
	if execCtx == nil || scoreCtx == nil || m == nil || cfg == nil || cfg.NewModel == nil {
		return fail("contexts, manifest and model factory are required")
	}
	if m.Version != 1 || m.RunID == "" || m.Scenario == "" || m.TaskSetVersion == "" || m.SourceRevision == "" {
		return fail("version 1 and nonempty run/scenario/task-set/source identities are required")
	}
	if !validScore(m.QualityThreshold) || m.QualityThreshold == 0 {
		return fail("quality threshold must be finite and in (0,1]")
	}
	if cfg.MaxInFlight <= 0 || cfg.MaxScorers <= 0 || cfg.MaxPendingScores <= 0 || cfg.TaskTimeout <= 0 || cfg.DrainTimeout <= 0 || cfg.ScoreTimeout <= 0 || cfg.ScorePhaseTimeout <= 0 {
		return fail("all load limits must be positive")
	}
	if len(m.Tasks) == 0 || len(m.Arrivals) == 0 || len(m.Tasks) > cfg.MaxPendingScores || len(m.Arrivals) > cfg.MaxPendingScores {
		return fail("tasks and arrivals must fit MaxPendingScores")
	}
	out := *m
	out.Tasks = make([]TaskSpec, len(m.Tasks))
	out.Arrivals = append([]TaskArrival(nil), m.Arrivals...)
	out.Metadata = make(map[string]string, len(m.Metadata))
	for k, v := range m.Metadata {
		out.Metadata[k] = v
	}
	tasks := make(map[string]TaskSpec, len(m.Tasks))
	for j := range m.Tasks {
		t := &m.Tasks[j]
		if math.IsNaN(t.Budget.MaxCostUSD) || math.IsInf(t.Budget.MaxCostUSD, 0) {
			return fail("task cost budgets must be finite")
		}
		if v := t.Sampling.Temperature; v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0)) {
			return fail("task sampling temperature must be finite")
		}
		if t.ID == "" || t.Input == "" {
			return fail("task ID and input are required")
		}
		if _, ok := tasks[t.ID]; ok {
			return fail("duplicate task ID")
		}
		out.Tasks[j] = cloneTask(t)
		tasks[t.ID] = out.Tasks[j]
	}
	type identity struct {
		id     string
		repeat int
	}
	seen := make(map[identity]bool)
	var previous time.Duration
	for _, a := range out.Arrivals {
		if _, ok := tasks[a.TaskID]; !ok {
			return fail("arrival references unknown task")
		}
		id := identity{a.TaskID, a.Repeat}
		if a.Repeat < 1 || seen[id] {
			return fail("arrival task/repeat must be positive and unique")
		}
		seen[id] = true
		if a.Offset < previous || a.Offset > math.MaxInt64-cfg.TaskTimeout || a.Offset > math.MaxInt64-cfg.DrainTimeout {
			return fail("arrival offsets must be nonnegative, nondecreasing and not overflow deadlines")
		}
		previous = a.Offset
	}
	return out, tasks, nil
}
func validScore(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1 }

func summarizeLoad(r *LoadReport, deadline time.Duration) {
	var latency []time.Duration
	passes := 0
	for j := range r.Results {
		row := &r.Results[j]
		if !row.Arrival.FinishedAt.IsZero() {
			latency = append(latency, row.Arrival.Latency)
		}
		if row.QualityStatus == QualityScored && row.Task.Pass && row.Arrival.Status == bench.OpenLoopSucceeded && row.Arrival.Latency < deadline {
			passes++
		}
	}
	r.CompletionSamples = len(latency)
	if len(latency) > 0 {
		sort.Slice(latency, func(i, j int) bool { return latency[i] < latency[j] })
		// Incremental floating sum avoids duration overflow across large samples.
		mean := 0.0
		for j, v := range latency {
			mean += (float64(v) - mean) / float64(j+1)
		}
		percentile := func(p float64) time.Duration {
			n := int(math.Ceil(p*float64(len(latency)))) - 1
			if n < 0 {
				n = 0
			}
			return latency[n]
		}
		r.Latencies = &bench.LatencyStats{Min: latency[0], Max: latency[len(latency)-1], Mean: time.Duration(mean), P50: percentile(.5), P95: percentile(.95), P99: percentile(.99)}
	}
	if seconds := r.Execution.EndTime.Sub(r.Execution.StartTime).Seconds(); seconds > 0 {
		v := float64(passes) / seconds
		r.Goodput = &v
	}
}

func errorDetail(err error, fallback string) string {
	if detail := err.Error(); detail != "" {
		return detail
	}
	return fallback
}

func cloneWorkload(m *WorkloadManifest) WorkloadManifest {
	out := *m
	out.Tasks = make([]TaskSpec, len(m.Tasks))
	for j := range m.Tasks {
		out.Tasks[j] = cloneTask(&m.Tasks[j])
	}
	out.Arrivals = append([]TaskArrival(nil), m.Arrivals...)
	out.Metadata = make(map[string]string, len(m.Metadata))
	for key, value := range m.Metadata {
		out.Metadata[key] = value
	}
	return out
}

// joinLoadExecution enforces the driver's observation interval even if this
// observer was preempted between the driver returning and freezing candidates.
func joinLoadExecution(report *LoadReport, candidates []*taskExecution, entered []time.Time) map[int]*taskExecution {
	pending := make(map[int]*taskExecution)
	for j, arrival := range report.Execution.Results {
		if entered[j].After(report.Execution.EndTime) {
			entered[j] = time.Time{}
		}
		if e := candidates[j]; e != nil && e.completedAt.After(report.Execution.EndTime) {
			e.close()
			candidates[j] = nil
		}
		row := &report.Results[j]
		row.Iteration = arrival.Iteration
		row.Arrival = arrival
		row.CallbackEnteredAt = entered[j]
		row.Task = TaskResult{TaskID: report.Manifest.Arrivals[j].TaskID, Repeat: report.Manifest.Arrivals[j].Repeat}
		row.QualityStatus = QualityNotExecuted
		if !arrival.StartedAt.IsZero() {
			report.Authorized++
		}
		if !entered[j].IsZero() {
			report.CallbackEntered++
			row.QualityStatus = QualityExecutionUnfinished
		}
		if e := candidates[j]; e != nil {
			row.Task = e.result
			row.Task.Trajectory = append([]string(nil), e.result.Trajectory...)
			row.ExecutionFinishedAt = e.completedAt
			if arrival.Status == bench.OpenLoopSucceeded && e.result.Error == "" {
				row.QualityStatus = QualityUnscored
				pending[j] = e
			} else {
				row.QualityStatus = QualityExecutionFailed
				if arrival.FinishedAt.IsZero() {
					row.QualityStatus = QualityExecutionUnfinished
				}
				e.close()
			}
		}
	}
	return pending
}
