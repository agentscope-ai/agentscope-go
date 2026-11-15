package evalkit

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/bench"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/managedtask"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func managedFixture() (*WorkloadManifest, *ManagedLoadConfig) {
	m := &WorkloadManifest{Version: 1, RunID: "managed-test", Scenario: "fixed", TaskSetVersion: "test-v1", SourceRevision: "fixture", QualityThreshold: 1,
		Tasks:    []TaskSpec{{ID: "a", Input: "approved text", Scorer: ScorerSpec{Ref: "contains", Expect: "correct"}}, {ID: "b", Input: "second text", Scorer: ScorerSpec{Ref: "contains", Expect: "correct"}}},
		Arrivals: []TaskArrival{{TaskID: "a", Repeat: 1}, {TaskID: "b", Repeat: 1, Offset: 20 * time.Millisecond}}}
	cfg := &ManagedLoadConfig{MaxInFlight: 2, TaskTimeout: time.Second, DrainTimeout: time.Second, MaxPendingScores: 10, MaxScorers: 2, ScoreTimeout: time.Second, ScorePhaseTimeout: time.Second, Categories: map[string]string{"a": "extract", "b": "rewrite"}}
	return m, cfg
}

func TestManagedLoadAccountingAndQuality(t *testing.T) {
	m, cfg := managedFixture()
	cfg.Execute = func(_ context.Context, task managedtask.Task) *managedtask.Result {
		if task.Category != "" || task.Mode != managedtask.Evaluation || task.ID == "a" {
			t.Errorf("gold category leaked or invalid evaluation identity: %+v", task)
		}
		if task.Text == "second text" {
			return &managedtask.Result{TaskID: task.ID, Status: managedtask.Rejected, Cost: managedtask.Cost{Currency: "USD", KnownSubtotal: 2, UnknownAttempts: 1}, Attempts: []managedtask.Attempt{{Kind: managedtask.PhysicalAttempt, OperationID: task.ID + "/classification", Completed: true}}}
		}
		return &managedtask.Result{TaskID: task.ID, Status: managedtask.Succeeded, Text: "correct", Cost: managedtask.Cost{Currency: "USD", KnownSubtotal: 3}, Attempts: []managedtask.Attempt{{Kind: managedtask.SimulatedAttempt, OperationID: "classification", Completed: true}, {Kind: managedtask.PhysicalAttempt, Completed: true, Err: errors.New("fixture failure")}, {Kind: managedtask.PhysicalAttempt, Completed: true}}}
	}
	r, err := new(Runner).RunManagedLoad(context.Background(), context.Background(), m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := r.Overall
	if s.Planned != 2 || s.Completed != 1 || s.Qualified != 1 || s.PhysicalRequests != 3 || s.SimulatedAttempts != 1 || s.ClassificationRequests != 1 || s.AdditionalGenerationRequests != 1 || s.KnownCostByCurrency["USD"] != 5 || s.KnownCostPerQualified["USD"] != 5 || s.CostPerQualified != nil || s.UnknownCostAttempts != 1 {
		t.Fatalf("accounting: %+v", s)
	}
	if r.Categories["rewrite"].Qualified != 0 || r.Categories["rewrite"].CostPerQualified != nil || r.Load.Results[1].QualityStatus != QualityExecutionFailed {
		t.Fatal("rejection lost")
	}
	if len(r.Results) != 2 || r.Results[1].Result.Status != managedtask.Rejected {
		t.Fatal("terminal result lost")
	}
	// Mutating caller inputs after return must not alter report metadata.
	cfg.Categories["a"] = "changed"
	m.Tasks[0].Input = "changed"
	if r.Results[0].Category != "extract" || r.Load.Manifest.Tasks[0].Input != "approved text" {
		t.Fatal("report aliases inputs")
	}
}

func TestManagedLoadPreservesInterruptedArrivals(t *testing.T) {
	m, cfg := managedFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg.Execute = func(context.Context, managedtask.Task) *managedtask.Result {
		t.Fatal("canceled workload dispatched")
		return nil
	}
	r, err := new(Runner).RunManagedLoad(ctx, context.Background(), m, cfg)
	if !errors.Is(err, context.Canceled) || len(r.Results) != 2 || r.Overall.Planned != 2 || r.Overall.Qualified != 0 || r.Overall.CostPerQualified != nil {
		t.Fatalf("partial: %+v %v", r, err)
	}
	for _, row := range r.Load.Results {
		if row.Arrival.Status != bench.OpenLoopNotOffered {
			t.Fatal(row.Arrival.Status)
		}
	}
}

func TestManagedLoadRejectsUnsupportedSpecifications(t *testing.T) {
	for _, mutate := range []func(*WorkloadManifest, *ManagedLoadConfig){
		func(m *WorkloadManifest, _ *ManagedLoadConfig) { m.Tasks[0].Tools = []string{"Bash"} },
		func(m *WorkloadManifest, _ *ManagedLoadConfig) { m.Tasks[0].System = "ignored" },
		func(m *WorkloadManifest, _ *ManagedLoadConfig) { m.Tasks[0].Turns = []string{"ignored"} },
		func(m *WorkloadManifest, _ *ManagedLoadConfig) { m.Tasks[0].Sampling.Seed = new(int64) },
		func(m *WorkloadManifest, _ *ManagedLoadConfig) { m.Tasks[0].Budget.MaxIters = 1 },
		func(_ *WorkloadManifest, c *ManagedLoadConfig) { delete(c.Categories, "a") },
		func(_ *WorkloadManifest, c *ManagedLoadConfig) { c.Execute = nil },
	} {
		m, cfg := managedFixture()
		cfg.Execute = func(context.Context, managedtask.Task) *managedtask.Result {
			t.Fatal("invalid specification dispatched")
			return nil
		}
		mutate(m, cfg)
		if r, err := new(Runner).RunManagedLoad(context.Background(), context.Background(), m, cfg); err == nil || r != nil {
			t.Fatal("unsupported configuration accepted")
		}
	}
}

func TestManagedSummaryZeroUnknownAndMixedCurrency(t *testing.T) {
	m, cfg := managedFixture()
	cfg.Execute = func(_ context.Context, task managedtask.Task) *managedtask.Result {
		currency := "USD"
		if task.Text == "second text" {
			currency = "CNY"
		}
		return &managedtask.Result{TaskID: task.ID, Status: managedtask.Succeeded, Text: "correct", Cost: managedtask.Cost{Currency: currency, KnownSubtotal: 2}}
	}
	r, err := new(Runner).RunManagedLoad(context.Background(), context.Background(), m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.Overall.CostPerQualified != nil || r.Overall.KnownCostPerQualified["USD"] != 1 || r.Overall.KnownCostPerQualified["CNY"] != 1 {
		t.Fatalf("mixed currencies: %+v", r.Overall)
	}
	for _, s := range r.Categories {
		if s.CostPerQualified == nil || *s.CostPerQualified != 2 {
			t.Fatal(s)
		}
	}
	cfg.Execute = func(_ context.Context, task managedtask.Task) *managedtask.Result {
		return &managedtask.Result{TaskID: task.ID, Status: managedtask.Succeeded, Text: "wrong"}
	}
	r, err = new(Runner).RunManagedLoad(context.Background(), context.Background(), m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.Overall.Qualified != 0 || r.Overall.CostPerQualified != nil || math.IsNaN(r.Overall.QualityPerPlanned) {
		t.Fatal(r.Overall)
	}
}

func TestManagedLoadFreezesLateExecutionAndGeneratorRejection(t *testing.T) {
	m, cfg := managedFixture()
	m.Arrivals[1].Offset = 0
	cfg.MaxInFlight = 1
	cfg.DrainTimeout = 100 * time.Millisecond
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	cfg.Execute = func(context.Context, managedtask.Task) *managedtask.Result {
		close(entered)
		<-release
		close(done)
		return &managedtask.Result{Status: managedtask.Succeeded, Text: "correct", Cost: managedtask.Cost{Currency: "USD", KnownSubtotal: 42}}
	}
	r, err := new(Runner).RunManagedLoad(context.Background(), context.Background(), m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if r.Load.Results[0].Arrival.Status != bench.OpenLoopUnfinished || r.Load.Results[1].Arrival.Status != bench.OpenLoopRejected || r.Overall.UnobservedExecutions != 1 || r.Overall.CostPerQualified != nil {
		t.Fatalf("lost incomplete observation: %+v", r.Overall)
	}
	close(release)
	<-done
	if r.Results[0].Result != nil || len(r.Overall.KnownCostByCurrency) != 0 {
		t.Fatal("late work mutated report")
	}
}

func TestManagedLoadIndependentScoringAndDeepCopy(t *testing.T) {
	m, cfg := managedFixture()
	m.Arrivals = m.Arrivals[:1]
	var runCtx context.Context
	tokens := int64(7)
	original := &managedtask.Result{Status: managedtask.Succeeded, Text: "correct", Attempts: []managedtask.Attempt{{Kind: managedtask.PhysicalAttempt, Completed: true, Usage: &model.TextUsage{InputTokens: &tokens, OutputTokens: &tokens, CachedInputTokens: &tokens}}}, Cost: managedtask.Cost{Currency: "USD", KnownSubtotal: 1}}
	cfg.Execute = func(ctx context.Context, _ managedtask.Task) *managedtask.Result { runCtx = ctx; return original }
	cfg.Scorer = loadScorerFunc(func(ctx context.Context, _ *TaskSpec, out *TaskOutcome) (float64, error) {
		if runCtx.Err() == nil || ctx.Err() != nil || out.FinalText != "correct" || out.Workspace != "" {
			t.Error("invalid scoring boundary")
		}
		original.Text = "changed"
		tokens = 99
		return 1, nil
	})
	r, err := new(Runner).RunManagedLoad(context.Background(), context.Background(), m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	result := r.Results[0].Result
	if result.Text != "correct" || *result.Attempts[0].Usage.InputTokens != 7 || *result.Attempts[0].Usage.OutputTokens != 7 || *result.Attempts[0].Usage.CachedInputTokens != 7 || r.Overall.CostPerQualified == nil {
		t.Fatal("result aliases execution")
	}
}

func TestManagedLoadNilFailureAndTimeoutResults(t *testing.T) {
	for _, result := range []*managedtask.Result{nil, {Status: managedtask.Failed}, {Status: managedtask.Canceled}, {Status: managedtask.TimedOut}, {Status: managedtask.Succeeded, Err: errors.New("bad result")}} {
		m, cfg := managedFixture()
		cfg.Execute = func(context.Context, managedtask.Task) *managedtask.Result { return result }
		r, err := new(Runner).RunManagedLoad(context.Background(), context.Background(), m, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if r.Overall.Qualified != 0 || r.Overall.Completed != 0 || r.Load.Results[0].QualityStatus == QualityScored {
			t.Fatal("failed result was scored")
		}
		if result == nil && r.Overall.UnobservedExecutions != 2 {
			t.Fatal("nil result treated as free")
		}
	}
}

func TestManagedLoadScoreErrorsAndUnfinishedAttempts(t *testing.T) {
	m, cfg := managedFixture()
	cfg.Execute = func(context.Context, managedtask.Task) *managedtask.Result {
		return &managedtask.Result{Status: managedtask.Succeeded, Text: "correct", Attempts: []managedtask.Attempt{{Kind: managedtask.PhysicalAttempt, Completed: false}}, Cost: managedtask.Cost{Currency: "USD", KnownSubtotal: 1}}
	}
	cfg.Scorer = loadScorerFunc(func(context.Context, *TaskSpec, *TaskOutcome) (float64, error) { return math.NaN(), nil })
	r, err := new(Runner).RunManagedLoad(context.Background(), context.Background(), m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.Overall.Qualified != 0 || r.Overall.UnfinishedAttempts != 2 || r.Load.Results[0].QualityStatus != QualityScoreError || r.Overall.MeanScoredQuality != nil {
		t.Fatal("invalid score accepted")
	}
	cfg.Scorer = nil
	r, err = new(Runner).RunManagedLoad(context.Background(), context.Background(), m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.Overall.Qualified != 2 || r.Overall.CostPerQualified != nil {
		t.Fatal("incomplete attempts have full cost")
	}
}

func TestManagedLoadAdditionalValidation(t *testing.T) {
	for _, mutate := range []func(*WorkloadManifest, *ManagedLoadConfig){
		func(m *WorkloadManifest, _ *ManagedLoadConfig) { m.Tasks[0].Fixture = "ignored" },
		func(m *WorkloadManifest, _ *ManagedLoadConfig) { m.Tasks[0].Repeat = 2 },
		func(m *WorkloadManifest, _ *ManagedLoadConfig) { m.Tasks[0].Scorer.Ref = "budget" },
		func(m *WorkloadManifest, _ *ManagedLoadConfig) { m.Version = 0 },
		func(m *WorkloadManifest, _ *ManagedLoadConfig) { m.Arrivals[0].TaskID = "missing" },
		func(_ *WorkloadManifest, c *ManagedLoadConfig) { c.MaxInFlight = 0 },
	} {
		m, cfg := managedFixture()
		cfg.Execute = func(context.Context, managedtask.Task) *managedtask.Result { t.Fatal("invalid plan ran"); return nil }
		mutate(m, cfg)
		if _, err := new(Runner).RunManagedLoad(context.Background(), context.Background(), m, cfg); err == nil {
			t.Fatal("invalid plan accepted")
		}
	}
	if _, err := new(Runner).RunManagedLoad(context.Background(), context.Background(), nil, nil); err == nil {
		t.Fatal("nil config accepted")
	}
}

func TestManagedObservationJSON(t *testing.T) {
	for _, row := range []ManagedObservation{{TaskID: "unobserved", Category: "extract"}, {TaskID: "failed", Category: "extract", Result: &managedtask.Result{Status: managedtask.Failed, Err: managedtask.ErrDenied, Attempts: []managedtask.Attempt{{Kind: managedtask.PhysicalAttempt, Completed: true, Err: managedtask.ErrDenied}, {Kind: managedtask.SimulatedAttempt, Completed: true}}}}} {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			TaskID string `json:"task_id"`
			Result *struct {
				Err      string
				Attempts []struct {
					Err  string
					Kind managedtask.AttemptKind
				}
			} `json:"result"`
		}
		if err = json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if row.Result == nil {
			if got.Result != nil {
				t.Fatal("unobserved result was replaced")
			}
			continue
		}
		if got.Result.Err != managedtask.ErrDenied.Error() || got.Result.Attempts[0].Err != managedtask.ErrDenied.Error() || got.Result.Attempts[1].Err != "" || got.Result.Attempts[1].Kind != managedtask.SimulatedAttempt {
			t.Fatalf("error/kind serialization: %s", raw)
		}
	}
}
