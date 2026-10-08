package evalkit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/bench"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

type loadScorerFunc func(context.Context, *TaskSpec, *TaskOutcome) (float64, error)

func (f loadScorerFunc) Score(ctx context.Context, t *TaskSpec, o *TaskOutcome) (float64, error) {
	return f(ctx, t, o)
}
func loadFixture() (*WorkloadManifest, *LoadConfig) {
	manifest := &WorkloadManifest{Version: 1, RunID: "run", Scenario: "offline", TaskSetVersion: "v1", SourceRevision: "fixture", QualityThreshold: 1, Tasks: []TaskSpec{{ID: "a", Input: "test", Scorer: ScorerSpec{Ref: "budget"}}}, Arrivals: []TaskArrival{{TaskID: "a", Repeat: 1, Offset: 0}}}
	cfg := &LoadConfig{MaxInFlight: 1, TaskTimeout: time.Second, DrainTimeout: time.Second, MaxPendingScores: 10, MaxScorers: 1, ScoreTimeout: time.Second, ScorePhaseTimeout: time.Second, NewModel: func(context.Context, TaskSpec) (model.ChatModel, error) { return &scriptedModel{}, nil }}
	return manifest, cfg
}
func TestRunLoadScoresAfterExecutionCleanupCancellation(t *testing.T) {
	m, cfg := loadFixture()
	var executionCtx context.Context
	var path string
	cfg.NewModel = func(ctx context.Context, _ TaskSpec) (model.ChatModel, error) {
		executionCtx = ctx
		// Goodput is documented as absent for a zero-length observation
		// interval. Keep the window wider than coarse-granularity clocks
		// (Windows CI runners) so the positive-goodput assertion stays
		// meaningful.
		time.Sleep(20 * time.Millisecond)
		return &scriptedModel{}, nil
	}
	cfg.Scorer = loadScorerFunc(func(ctx context.Context, _ *TaskSpec, out *TaskOutcome) (float64, error) {
		if executionCtx.Err() == nil {
			t.Error("bench callback context should be canceled before scoring")
		}
		if ctx.Err() != nil {
			t.Error("scoring inherited callback cancellation")
		}
		path = out.Workspace
		if _, err := os.Stat(path); err != nil {
			t.Error("workspace removed before scoring")
		}
		return 1, nil
	})
	report, err := (&Runner{}).RunLoad(context.Background(), context.Background(), m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	row := report.Results[0]
	if row.Arrival.Status != bench.OpenLoopSucceeded || row.QualityStatus != QualityScored || !row.Task.Pass {
		t.Fatalf("row=%+v", row)
	}
	if row.Iteration != 1 || row.Task.TaskID != "a" || row.Task.Repeat != 1 || report.Goodput == nil || *report.Goodput <= 0 {
		t.Fatalf("report=%+v", report)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace leaked: %v", err)
	}
}
func TestRunLoadRetainsRejectedArrivals(t *testing.T) {
	m, cfg := loadFixture()
	m.Arrivals = append(m.Arrivals, TaskArrival{TaskID: "a", Repeat: 2, Offset: 0})
	release := make(chan struct{})
	cfg.NewModel = func(ctx context.Context, _ TaskSpec) (model.ChatModel, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return &scriptedModel{}, nil
		}
	}
	// Both arrivals are simultaneous; the first factory stays active until its deadline.
	cfg.TaskTimeout = 500 * time.Millisecond
	cfg.DrainTimeout = time.Second
	r, err := (&Runner{}).RunLoad(context.Background(), context.Background(), m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Results) != 2 || r.Results[1].Arrival.Status != bench.OpenLoopRejected || r.Results[1].QualityStatus != QualityNotExecuted {
		t.Fatalf("results=%+v", r.Results)
	}
	if r.Planned != 2 || r.Offered != 2 || r.Authorized != 1 {
		t.Fatalf("counts=%+v", r)
	}
}
func TestRunLoadBoundsActualScorersAndFreezesReport(t *testing.T) {
	m, cfg := loadFixture()
	cfg.MaxInFlight = 4
	for n := 2; n <= 4; n++ {
		m.Arrivals = append(m.Arrivals, TaskArrival{TaskID: "a", Repeat: n, Offset: 0})
	}
	cfg.ScoreTimeout = 25 * time.Millisecond
	cfg.ScorePhaseTimeout = 500 * time.Millisecond
	release, exited := make(chan struct{}), make(chan struct{})
	workspace := make(chan string, 1)
	var calls atomic.Int32
	cfg.Scorer = loadScorerFunc(func(_ context.Context, _ *TaskSpec, out *TaskOutcome) (float64, error) {
		workspace <- out.Workspace
		calls.Add(1)
		<-release
		close(exited)
		return 1, nil
	})
	r, err := (&Runner{}).RunLoad(context.Background(), context.Background(), m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("started %d uncooperative scorers", calls.Load())
	}
	for _, row := range r.Results {
		if row.Task.Pass || row.QualityStatus == QualityScored {
			t.Fatalf("late score accepted: %+v", row)
		}
	}
	close(release)
	<-exited
	waitWorkspaceRemoved(t, <-workspace)
	for _, row := range r.Results {
		if row.Task.Pass {
			t.Fatal("published report mutated")
		}
	}
}
func TestRunLoadValidation(t *testing.T) {
	m, cfg := loadFixture()
	m.Version = 2
	if _, err := (&Runner{}).RunLoad(context.Background(), context.Background(), m, cfg); err == nil {
		t.Fatal("accepted future version")
	}
	m.Version = 1
	cfg.MaxPendingScores = 0
	if _, err := (&Runner{}).RunLoad(context.Background(), context.Background(), m, cfg); err == nil {
		t.Fatal("accepted unbounded retention")
	}
}

func TestRunLoadSeparatesApplicationRejectionAndProviderFailure(t *testing.T) {
	for _, admission := range []bool{false, true} {
		m, cfg := loadFixture()
		cfg.NewModel = func(context.Context, TaskSpec) (model.ChatModel, error) {
			if admission {
				return nil, ErrAdmissionRejected
			}
			return &terminalModel{}, nil
		}
		r, err := (&Runner{}).RunLoad(context.Background(), context.Background(), m, cfg)
		if err != nil {
			t.Fatal(err)
		}
		row := r.Results[0]
		if row.Arrival.Status != bench.OpenLoopFailed || row.QualityStatus != QualityExecutionFailed || row.Task.Pass {
			t.Fatalf("row=%+v", row)
		}
		if admission && row.Task.ErrorType != "admission_rejected" {
			t.Fatalf("kind=%q", row.Task.ErrorType)
		}
		if !admission && row.Task.ErrorType == "admission_rejected" {
			t.Fatal("provider confused with admission")
		}
	}
}
func TestRunLoadScoreErrorsAndThreshold(t *testing.T) {
	for _, score := range []float64{0.5, 1.1, math.NaN()} {
		m, cfg := loadFixture()
		cfg.Scorer = loadScorerFunc(func(context.Context, *TaskSpec, *TaskOutcome) (float64, error) { return score, nil })
		r, err := (&Runner{}).RunLoad(context.Background(), context.Background(), m, cfg)
		if err != nil {
			t.Fatal(err)
		}
		row := r.Results[0]
		if row.Task.Pass {
			t.Fatal("nonpassing score passed")
		}
		if score == 0.5 && row.QualityStatus != QualityScored {
			t.Fatal("valid low score treated as error")
		}
		if score != 0.5 && row.QualityStatus != QualityScoreError {
			t.Fatal("invalid score accepted")
		}
	}
}
func TestRunLoadInvalidManifests(t *testing.T) {
	cases := []func(*WorkloadManifest, *LoadConfig){
		func(m *WorkloadManifest, _ *LoadConfig) { m.QualityThreshold = math.Inf(1) },
		func(m *WorkloadManifest, _ *LoadConfig) { m.QualityThreshold = 0 },
		func(m *WorkloadManifest, _ *LoadConfig) { m.RunID = "" },
		func(m *WorkloadManifest, _ *LoadConfig) { m.Tasks = append(m.Tasks, m.Tasks[0]) },
		func(m *WorkloadManifest, _ *LoadConfig) { m.Tasks[0].Input = "" },
		func(m *WorkloadManifest, _ *LoadConfig) { m.Arrivals[0].TaskID = "unknown" },
		func(m *WorkloadManifest, _ *LoadConfig) { m.Arrivals[0].Repeat = 0 },
		func(m *WorkloadManifest, _ *LoadConfig) { m.Arrivals = append(m.Arrivals, m.Arrivals[0]) },
		func(m *WorkloadManifest, _ *LoadConfig) { m.Arrivals[0].Offset = -1 },
		func(m *WorkloadManifest, _ *LoadConfig) { m.Arrivals[0].Offset = math.MaxInt64 },
		func(_ *WorkloadManifest, c *LoadConfig) { c.NewModel = nil },
		func(_ *WorkloadManifest, c *LoadConfig) { c.MaxInFlight = 0 },
		func(m *WorkloadManifest, c *LoadConfig) {
			c.MaxPendingScores = 1
			m.Arrivals = append(m.Arrivals, TaskArrival{TaskID: "a", Repeat: 2})
		},
	}
	for j, change := range cases {
		m, cfg := loadFixture()
		change(m, cfg)
		if _, err := (&Runner{}).RunLoad(context.Background(), context.Background(), m, cfg); err == nil {
			t.Fatalf("case %d accepted", j)
		}
	}
	m, cfg := loadFixture()
	var nilContext context.Context
	if _, err := (&Runner{}).RunLoad(nilContext, context.Background(), m, cfg); err == nil {
		t.Fatal("nil context accepted")
	}
	if _, err := (&Runner{}).RunLoad(context.Background(), nilContext, m, cfg); err == nil {
		t.Fatal("nil score context accepted")
	}
	if _, err := (&Runner{}).RunLoad(context.Background(), context.Background(), nil, cfg); err == nil {
		t.Fatal("nil manifest accepted")
	}
}
func TestRunLoadCanceledBeforeObservation(t *testing.T) {
	m, cfg := loadFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := (&Runner{}).RunLoad(ctx, context.Background(), m, cfg)
	if !errors.Is(err, context.Canceled) || r.Goodput != nil || r.CompletionSamples != 0 || r.Results[0].QualityStatus != QualityNotExecuted {
		t.Fatalf("report=%+v err=%v", r, err)
	}
}

type emptyLoadError struct{}

func (emptyLoadError) Error() string { return "" }
func TestRunLoadEmptyErrorStillFails(t *testing.T) {
	for _, factory := range []bool{false, true} {
		m, cfg := loadFixture()
		if factory {
			cfg.NewModel = func(context.Context, TaskSpec) (model.ChatModel, error) { return &scriptedModel{}, emptyLoadError{} }
		} else {
			cfg.Scorer = loadScorerFunc(func(context.Context, *TaskSpec, *TaskOutcome) (float64, error) { return 1, emptyLoadError{} })
		}
		r, err := (&Runner{}).RunLoad(context.Background(), context.Background(), m, cfg)
		if err != nil {
			t.Fatal(err)
		}
		row := r.Results[0]
		if row.Task.Pass || row.QualityStatus == QualityScored {
			t.Fatalf("empty error passed: %+v", row)
		}
		if factory && row.Task.Error == "" {
			t.Fatal("execution lacks error diagnostic")
		}
		if !factory && row.Task.ScoreError == "" {
			t.Fatal("scoring lacks error diagnostic")
		}
	}
}

func TestRunLoadFactoryPreservesCancellationClass(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		m, cfg := loadFixture()
		cfg.NewModel = func(context.Context, TaskSpec) (model.ChatModel, error) { return nil, fmt.Errorf("factory: %w", cause) }
		r, err := (&Runner{}).RunLoad(context.Background(), context.Background(), m, cfg)
		if err != nil {
			t.Fatal(err)
		}
		want := bench.OpenLoopCanceled
		if cause == context.DeadlineExceeded {
			want = bench.OpenLoopTimedOut
		}
		if r.Results[0].Arrival.Status != want {
			t.Fatalf("cause=%v arrival=%+v", cause, r.Results[0].Arrival)
		}
	}
}
func TestRunLoadExpiredScoreContextHasZeroInterval(t *testing.T) {
	m, cfg := loadFixture()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	r, err := (&Runner{}).RunLoad(context.Background(), ctx, m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !r.ScoringFinishedAt.Equal(r.ScoringStartedAt) || r.Results[0].QualityStatus != QualityUnscored {
		t.Fatalf("invalid score interval: %s to %s status=%s", r.ScoringStartedAt, r.ScoringFinishedAt, r.Results[0].QualityStatus)
	}
}
func TestRunLoadRejectsNonfiniteTaskSettings(t *testing.T) {
	for _, sampling := range []bool{false, true} {
		m, cfg := loadFixture()
		invalid := math.NaN()
		if sampling {
			m.Tasks[0].Sampling.Temperature = &invalid
		} else {
			m.Tasks[0].Budget.MaxCostUSD = invalid
		}
		if _, err := (&Runner{}).RunLoad(context.Background(), context.Background(), m, cfg); err == nil {
			t.Fatal("accepted unserializable manifest")
		}
	}
}

func TestRunLoadManifestOwnership(t *testing.T) {
	m, cfg := loadFixture()
	temp := 0.25
	seed := int64(42)
	m.Metadata = map[string]string{"source": "original"}
	m.Tasks[0].Tags = []string{"original"}
	m.Tasks[0].Turns = []string{"second"}
	m.Tasks[0].Scorer.Items = []string{"original"}
	m.Tasks[0].Sampling = SamplingSpec{Temperature: &temp, Seed: &seed}
	var captured TaskSpec
	cfg.NewModel = func(_ context.Context, task TaskSpec) (model.ChatModel, error) {
		captured = task
		task.Tags[0] = "factory"
		task.Turns[0] = "factory"
		task.Scorer.Items[0] = "factory"
		*task.Sampling.Temperature = 0.8
		*task.Sampling.Seed = 1
		return &scriptedModel{}, nil
	}
	r, err := (&Runner{}).RunLoad(context.Background(), context.Background(), m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []*TaskSpec{&m.Tasks[0], &r.Manifest.Tasks[0]} {
		if task.Tags[0] != "original" || task.Turns[0] != "second" || task.Scorer.Items[0] != "original" || *task.Sampling.Temperature != 0.25 || *task.Sampling.Seed != 42 {
			t.Fatalf("factory changed corpus: %+v", task)
		}
	}
	r.Manifest.Tasks[0].Tags[0] = "returned"
	r.Manifest.Tasks[0].Turns[0] = "returned"
	r.Manifest.Tasks[0].Scorer.Items[0] = "returned"
	*r.Manifest.Tasks[0].Sampling.Temperature = 0.9
	*r.Manifest.Tasks[0].Sampling.Seed = 9
	r.Manifest.Metadata["source"] = "returned"
	r.Manifest.Arrivals[0].TaskID = "returned"
	if captured.Tags[0] != "factory" || *captured.Sampling.Temperature != 0.8 || m.Metadata["source"] != "original" || m.Arrivals[0].TaskID != "a" {
		t.Fatal("returned corpus shares mutable input")
	}
}
