package evalkit

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/bench"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestRunLoadLateExecutionKeepsWorkspaceAndFrozenReport(t *testing.T) {
	entered, release, exited := installWorkspaceHold(t)
	m, cfg := loadFixture()
	m.Tasks[0].Input = "{workspace}"
	m.Tasks[0].Tools = []string{"hold_workspace"}
	cfg.NewModel = func(context.Context, TaskSpec) (model.ChatModel, error) { return &workspaceHoldModel{}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *LoadReport, 1)
	go func() {
		r, err := (&Runner{}).RunLoad(ctx, context.Background(), m, cfg)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("run error=%v", err)
		}
		done <- r
	}()
	var path string
	select {
	case path = <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("tool did not start")
	}
	cancel()
	var report *LoadReport
	select {
	case report = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("observation blocked by tool")
	}
	if report.Results[0].QualityStatus != QualityExecutionUnfinished {
		t.Fatalf("premature snapshot: %+v", report.Results[0])
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("workspace removed while execution active")
	}
	close(release)
	<-exited
	timeout := time.After(3 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		select {
		case <-ticker.C:
		case <-timeout:
			t.Fatal("late snapshot not cleaned")
		}
	}
	if report.Results[0].QualityStatus != QualityExecutionUnfinished || report.Results[0].Task.Pass {
		t.Fatal("late task changed published result")
	}
}

type heldScoreError struct{ release <-chan struct{} }

func (e heldScoreError) Error() string { <-e.release; return "delayed failure" }
func TestRunLoadBoundsScoreErrorInspection(t *testing.T) {
	m, cfg := loadFixture()
	m.Arrivals = append(m.Arrivals, TaskArrival{TaskID: "a", Repeat: 2, Offset: 0})
	cfg.MaxInFlight = 2
	cfg.ScorePhaseTimeout = 500 * time.Millisecond
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	workspace := make(chan string, 1)
	cfg.Scorer = loadScorerFunc(func(_ context.Context, _ *TaskSpec, out *TaskOutcome) (float64, error) {
		workspace <- out.Workspace
		started <- struct{}{}
		return 0, heldScoreError{release: release}
	})
	r, err := (&Runner{}).RunLoad(context.Background(), context.Background(), m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 {
		t.Fatalf("error inspection released scorer slot: %d calls", len(started))
	}
	close(release)
	waitWorkspaceRemoved(t, <-workspace)
	for _, row := range r.Results {
		if row.QualityStatus == QualityScored || row.Task.Pass {
			t.Fatal("accepted unfinished score")
		}
	}
}

func TestLoadJoinUsesDriverObservationInterval(t *testing.T) {
	origin := time.Now()
	end := origin.Add(time.Second)
	m, _ := loadFixture()
	m.Arrivals = append(m.Arrivals, TaskArrival{TaskID: "a", Repeat: 2})
	report := &LoadReport{Manifest: *m, Execution: &bench.OpenLoopReport{StartTime: origin, EndTime: end, Results: []bench.OpenLoopResult{{Iteration: 1, StartedAt: origin, Status: bench.OpenLoopUnfinished}, {Iteration: 2, StartedAt: origin, Status: bench.OpenLoopUnfinished}}}, Results: make([]TaskQualityResult, 2)}
	first, late := t.TempDir(), t.TempDir()
	candidates := []*taskExecution{{result: TaskResult{TaskID: "a", Repeat: 1}, workDir: first, completedAt: origin.Add(time.Millisecond)}, {result: TaskResult{TaskID: "a", Repeat: 2}, workDir: late, completedAt: end.Add(time.Millisecond)}}
	pending := joinLoadExecution(report, candidates, []time.Time{origin, end.Add(time.Millisecond)})
	if len(pending) != 0 || report.CallbackEntered != 1 || report.Results[0].QualityStatus != QualityExecutionUnfinished || !report.Results[1].CallbackEnteredAt.IsZero() {
		t.Fatalf("late candidate changed interval: %+v", report)
	}
	for _, path := range []string{first, late} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unaccepted candidate retained workspace: %s", path)
		}
	}
}
