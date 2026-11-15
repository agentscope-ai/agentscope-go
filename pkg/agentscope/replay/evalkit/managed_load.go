package evalkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/bench"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/managedtask"
)

// ManagedLoadConfig evaluates an existing managed Executor or Router, bypassing
// Agent entirely. Execute and Scorer must be concurrency-safe and honor context.
// Bind Execute to one shared instance to preserve its admission limits. The host
// must enforce evaluation isolation and approve/project all disclosed content.
// Categories are independent gold labels used only for reporting, never routing.
// Callers must not mutate configuration/manifest while they are copied.
type ManagedLoadConfig struct {
	MaxInFlight                     int
	TaskTimeout, DrainTimeout       time.Duration
	MaxPendingScores, MaxScorers    int
	ScoreTimeout, ScorePhaseTimeout time.Duration
	Execute                         func(context.Context, managedtask.Task) *managedtask.Result
	Scorer                          Scorer
	Categories                      map[string]string
}

// ManagedObservation retains a frozen managed result if observed before the
// execution cutoff. Nil means no observed result, not zero requests or zero cost.
// Result text and the manifest can contain sensitive data; store reports locally.
type ManagedObservation struct {
	TaskID   string              `json:"task_id"`
	Category string              `json:"category"`
	Result   *managedtask.Result `json:"result"`
}

// MarshalJSON renders error messages as strings rather than serializing error
// implementations. In-memory errors retain their identity; error values shared
// by Execute must be immutable, as in managedtask's own result contract.
func (o ManagedObservation) MarshalJSON() ([]byte, error) {
	type attemptAlias managedtask.Attempt
	type resultAlias managedtask.Result
	type attemptJSON struct {
		attemptAlias
		Err string
	}
	type resultJSON struct {
		resultAlias
		Attempts []attemptJSON
		Err      string
	}
	var result *resultJSON
	if o.Result != nil {
		result = &resultJSON{resultAlias: resultAlias(*o.Result), Attempts: make([]attemptJSON, len(o.Result.Attempts))}
		if o.Result.Err != nil {
			result.Err = o.Result.Err.Error()
		}
		for i := range o.Result.Attempts {
			a := &o.Result.Attempts[i]
			result.Attempts[i] = attemptJSON{attemptAlias: attemptAlias(*a)}
			if a.Err != nil {
				result.Attempts[i].Err = a.Err.Error()
			}
		}
	}
	return json.Marshal(struct {
		TaskID   string      `json:"task_id"`
		Category string      `json:"category"`
		Result   *resultJSON `json:"result"`
	}{o.TaskID, o.Category, result})
}

// ManagedSummary includes every planned task in quality/completion denominators.
// QualityPerPlanned treats unscored/non-successful tasks as zero; MeanScoredQuality
// uses only scored successes. Qualified also requires on-time execution success.
// Costs include ALL observed tasks, including classification/failures/fallbacks.
// KnownCostPerQualified is a subtotal, not a complete price. CostPerQualified is
// nil for zero qualified completions, multiple/no currencies, unknown attempts,
// or unobserved execution. Request counts are observed lower bounds if incomplete.
// ClassificationAttemptTime sums attempt durations, not task wall-clock overhead.
type ManagedSummary struct {
	Planned, Completed, Scored, Qualified                                    int
	CompletionRate, QualityPerPlanned                                        float64
	MeanScoredQuality                                                        *float64
	Statuses                                                                 map[managedtask.Status]int
	ArrivalStatuses                                                          map[bench.OpenLoopStatus]int
	PhysicalRequests, SimulatedAttempts                                      int
	ClassificationRequests, GenerationRequests, AdditionalGenerationRequests int
	ClassificationAttemptTime                                                time.Duration
	UnknownCostAttempts, UnobservedExecutions, UnfinishedAttempts            int
	KnownCostByCurrency                                                      map[string]float64
	KnownCostPerQualified                                                    map[string]float64
	CostPerQualified                                                         *float64
	LatencySamples                                                           int
	CompletionLatencies                                                      *bench.LatencyStats
}

// ManagedLoadReport joins managed accounting with existing arrival and quality
// observations. No qualification evidence is generated or certified by this API.
// Frozen reports cannot be updated by late callbacks/scorers; terminate any
// uncooperative host callbacks before repeated runs, as with RunLoad.
type ManagedLoadReport struct {
	Load       *LoadReport               `json:"load"`
	Results    []ManagedObservation      `json:"managed_results"`
	Overall    ManagedSummary            `json:"overall"`
	Categories map[string]ManagedSummary `json:"categories"`
}

// RunManagedLoad reuses open-loop load generation and bounded independent scoring
// on the managed entry point. All tasks use Evaluation, empty Category, and unique
// RunID/arrival IDs. A fixed-model wrapper may supply its own trusted category.
// Only Input and text scoring are supported: agent/system/tool/workspace/turn,
// budget and sampling settings are rejected rather than silently ignored.
// Runner's Agent-specific settings do not apply; limits are explicit in cfg and
// the bound managed instance. Returns all planned arrivals on interruption.
func (r *Runner) RunManagedLoad(execCtx, scoreCtx context.Context, manifest *WorkloadManifest, cfg *ManagedLoadConfig) (*ManagedLoadReport, error) {
	if cfg == nil || cfg.Execute == nil {
		return nil, fmt.Errorf("evalkit: managed Execute is required")
	}
	config := *cfg
	loadCfg := &LoadConfig{MaxInFlight: config.MaxInFlight, TaskTimeout: config.TaskTimeout, DrainTimeout: config.DrainTimeout, MaxPendingScores: config.MaxPendingScores, MaxScorers: config.MaxScorers, ScoreTimeout: config.ScoreTimeout, ScorePhaseTimeout: config.ScorePhaseTimeout, Scorer: config.Scorer}
	m, tasks, err := validateWorkload(execCtx, scoreCtx, manifest, loadCfg)
	if err != nil {
		return nil, err
	}
	categories := make(map[string]string, len(m.Tasks))
	for i := range m.Tasks {
		t := &m.Tasks[i]
		category := config.Categories[t.ID]
		if category == "" || len(t.Turns) > 0 || t.System != "" || len(t.Tools) > 0 || t.Fixture != "" || t.Budget != (BudgetSpec{}) || t.Sampling != (SamplingSpec{}) || t.Repeat < 0 || t.Repeat > 1 {
			return nil, fmt.Errorf("evalkit: managed tasks require gold categories and single plain text inputs without agent settings")
		}
		if config.Scorer == nil && (t.Scorer.Ref == "trajectory" || t.Scorer.Ref == "budget") {
			return nil, fmt.Errorf("evalkit: managed tasks require text scoring")
		}
		categories[t.ID] = category
	}
	state := &managedLoadState{open: true, candidates: make([]*taskExecution, len(m.Arrivals)), results: make([]*managedtask.Result, len(m.Arrivals)), entered: make([]time.Time, len(m.Arrivals))}
	offsets := make([]time.Duration, len(m.Arrivals))
	for i, a := range m.Arrivals {
		offsets[i] = a.Offset
	}
	scenario := &bench.OpenLoopScenario{Name: m.Scenario, ArrivalOffsets: offsets, MaxInFlight: config.MaxInFlight, Timeout: config.TaskTimeout, DrainTimeout: config.DrainTimeout}
	scenario.Run = func(ctx context.Context, iteration int) error {
		index := iteration - 1
		state.mu.Lock()
		if !state.open {
			state.mu.Unlock()
			return context.Canceled
		}
		state.entered[index] = time.Now()
		state.mu.Unlock()
		a := m.Arrivals[index]
		spec := tasks[a.TaskID]
		task := managedtask.Task{ID: fmt.Sprintf("%s/%d", m.RunID, iteration), Text: spec.Input, Mode: managedtask.Evaluation}
		result := cloneManagedResult(config.Execute(ctx, task))
		e := &taskExecution{task: cloneTask(&spec), result: TaskResult{TaskID: a.TaskID, Repeat: a.Repeat}, completedAt: time.Now()}
		if result == nil || result.Status != managedtask.Succeeded || result.Err != nil {
			e.result.Error = "managed execution did not succeed"
		} else {
			e.outcome.FinalText = result.Text
			e.result.Iters = len(result.Attempts)
		}
		state.mu.Lock()
		if state.open {
			state.candidates[index] = e
			state.results[index] = result
		}
		state.mu.Unlock()
		if e.result.Error != "" {
			if result != nil && result.Status == managedtask.TimedOut {
				return context.DeadlineExceeded
			}
			if result != nil && result.Status == managedtask.Canceled {
				return context.Canceled
			}
			return errors.New(e.result.Error)
		}
		return nil
	}
	execution, runErr := bench.NewRunner().RunOpenLoop(execCtx, scenario)
	if execution == nil {
		return nil, runErr
	}
	state.mu.Lock()
	state.open = false
	candidates, results, entered := state.candidates, state.results, state.entered
	state.candidates = nil
	state.results = nil
	state.entered = nil
	state.mu.Unlock()
	load := &LoadReport{Version: 1, Manifest: cloneWorkload(&m), Execution: execution, Planned: len(m.Arrivals), Offered: execution.Offered, Results: make([]TaskQualityResult, len(m.Arrivals)), Limits: LoadLimits{MaxInFlight: config.MaxInFlight, TaskTimeout: config.TaskTimeout, DrainTimeout: config.DrainTimeout, MaxPendingScores: config.MaxPendingScores, MaxScorers: config.MaxScorers, ScoreTimeout: config.ScoreTimeout, ScorePhaseTimeout: config.ScorePhaseTimeout}}
	report := &ManagedLoadReport{Load: load, Results: make([]ManagedObservation, len(m.Arrivals)), Categories: map[string]ManagedSummary{}}
	for i, a := range m.Arrivals {
		if e := candidates[i]; e == nil || e.completedAt.After(execution.EndTime) {
			results[i] = nil
		}
		report.Results[i] = ManagedObservation{TaskID: a.TaskID, Category: categories[a.TaskID], Result: results[i]}
	}
	pending := joinLoadExecution(load, candidates, entered)
	scoreLoad(scoreCtx, loadCfg, load, pending)
	summarizeLoad(load, config.TaskTimeout)
	report.summarize(config.TaskTimeout)
	return report, runErr
}

type managedLoadState struct {
	mu         sync.Mutex
	open       bool
	candidates []*taskExecution
	results    []*managedtask.Result
	entered    []time.Time
}

func cloneManagedResult(in *managedtask.Result) *managedtask.Result {
	if in == nil {
		return nil
	}
	out := *in
	out.Attempts = append([]managedtask.Attempt(nil), in.Attempts...)
	for i := range out.Attempts {
		if usage := out.Attempts[i].Usage; usage != nil {
			copyUsage := *usage
			copyUsage.InputTokens = cloneTokenCount(usage.InputTokens)
			copyUsage.OutputTokens = cloneTokenCount(usage.OutputTokens)
			copyUsage.CachedInputTokens = cloneTokenCount(usage.CachedInputTokens)
			out.Attempts[i].Usage = &copyUsage
		}
	}
	return &out
}

func cloneTokenCount(in *int64) *int64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
