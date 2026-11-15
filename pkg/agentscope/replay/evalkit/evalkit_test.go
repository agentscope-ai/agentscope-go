package evalkit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"gopkg.in/yaml.v3"
)

// scriptedModel replays canned responses for e2e tests.
type scriptedModel struct {
	responses []model.ChatResponse
	idx       int
	lastTemp  *float64
}

func (m *scriptedModel) Chat(_ context.Context, _ []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	var o model.CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	m.lastTemp = o.Temperature
	if m.idx >= len(m.responses) {
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true}, nil
	}
	resp := m.responses[m.idx]
	m.idx++
	return &resp, nil
}

func (m *scriptedModel) ChatStream(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (m *scriptedModel) CountTokens(_ []*message.Msg, _ []model.ToolSchema) int { return 10 }

func TestScorers(t *testing.T) {
	out := TaskOutcome{
		FinalText:  `The answer is {"count": 3} as computed.`,
		Trajectory: []string{"Read", "Grep", "Edit"},
		Iters:      4, InputTokens: 100, OutputTokens: 50,
	}
	ctx := context.Background()

	cases := []struct {
		name string
		spec ScorerSpec
		want float64
	}{
		{"contains-hit", ScorerSpec{Ref: "contains", Expect: "The answer"}, 1},
		{"contains-miss", ScorerSpec{Ref: "contains", Expect: "nope"}, 0},
		{"json_field-hit", ScorerSpec{Ref: "json_field", Field: "count", Expect: "3"}, 1},
		{"json_field-miss", ScorerSpec{Ref: "json_field", Field: "count", Expect: "9"}, 0},
		{"text_contains-partial", ScorerSpec{Ref: "text_contains", Items: []string{"answer", "zzz"}}, 0.5},
		{"trajectory-subseq", ScorerSpec{Ref: "trajectory", Items: []string{"Read", "Edit"}}, 1},
		{"trajectory-exact-miss", ScorerSpec{Ref: "trajectory", Items: []string{"Read", "Edit"}, Mode: "exact"}, 0},
		{"trajectory-exact-hit", ScorerSpec{Ref: "trajectory", Items: []string{"Read", "Grep", "Edit"}, Mode: "exact"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := BuildScorer(&tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.Score(ctx, &TaskSpec{}, &out)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("score = %v, want %v", got, tc.want)
			}
		})
	}

	// budget scorer
	b, err := BuildScorer(&ScorerSpec{Ref: "budget"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := b.Score(ctx, &TaskSpec{Budget: BudgetSpec{MaxIters: 3}}, &out)
	if got != 0 {
		t.Errorf("budget over-limit should fail, got %v", got)
	}
	got, _ = b.Score(ctx, &TaskSpec{Budget: BudgetSpec{MaxIters: 8, MaxInTokens: 1000}}, &out)
	if got != 1 {
		t.Errorf("budget within limit should pass, got %v", got)
	}
}

func TestTaskSpecYAMLRoundTrip(t *testing.T) {
	raw := `
id: demo-1
tags: [smoke]
input: count dates in {workspace}/a.txt
tools: [Read]
scorer: {ref: contains, expect: "3"}
budget: {max_iters: 5}
sampling: {temperature: 0}
repeat: 2
`
	var spec TaskSpec
	if err := yaml.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.ID != "demo-1" || spec.Scorer.Ref != "contains" || spec.Repeat != 2 ||
		spec.Sampling.Temperature == nil || *spec.Sampling.Temperature != 0 ||
		!spec.HasTag("smoke") || len(spec.Tools) != 1 {
		t.Errorf("unexpected parse: %+v", spec)
	}
}

func TestRunTask_EndToEnd_WithDeterminismAndTrajectory(t *testing.T) {
	// Fixture + custom tool: the model reads the fixture file via marker tool,
	// then answers; we assert trajectory and scoring end to end.
	RegisterToolFactory("marker_echo", func() tool.Tool {
		return tool.NewFunctionTool("marker_echo", "echoes the marker", nil,
			func(_ context.Context, _ map[string]any) (any, error) {
				return "MARKER-42", nil
			})
	})

	m := &scriptedModel{responses: []model.ChatResponse{
		{Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "tc1", Name: "marker_echo", Input: `{}`, State: message.ToolCallPending},
		}, IsLast: true},
		{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "found MARKER-42 in the file"}}, IsLast: true},
	}}

	fixture := t.TempDir()
	if err := os.WriteFile(filepath.Join(fixture, "a.txt"), []byte("x 2026-01-01 y"), 0o644); err != nil {
		t.Fatal(err)
	}

	task := TaskSpec{
		ID:      "e2e",
		Input:   "read {workspace}/a.txt and report",
		Tools:   []string{"marker_echo"},
		Scorer:  ScorerSpec{Ref: "trajectory", Items: []string{"marker_echo"}, Mode: "exact"},
		Budget:  BudgetSpec{MaxIters: 4},
		Fixture: fixture,
	}

	r := &Runner{}
	res := r.RunTask(context.Background(), &task, m)
	if res.Error != "" {
		t.Fatalf("task error: %s", res.Error)
	}
	if !res.Pass {
		t.Fatalf("task should pass: %+v", res)
	}
	if len(res.Trajectory) != 1 || res.Trajectory[0] != "marker_echo" {
		t.Errorf("trajectory = %v", res.Trajectory)
	}
	if res.Iters != 2 {
		t.Errorf("iters = %d, want 2", res.Iters)
	}
	// Determinism: sampling shim must pin temperature 0 by default.
	if m.lastTemp == nil || *m.lastTemp != 0 {
		t.Errorf("temperature not pinned to 0: %v", m.lastTemp)
	}
}

func TestRunSuite_MarkdownAndAggregates(t *testing.T) {
	m := &scriptedModel{responses: []model.ChatResponse{
		{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "the answer is YES"}}, IsLast: true},
	}}
	tasks := []TaskSpec{
		{ID: "t1", Input: "q1", Scorer: ScorerSpec{Ref: "contains", Expect: "YES"}},
		{ID: "t2", Input: "q2", Scorer: ScorerSpec{Ref: "contains", Expect: "NO"}},
	}
	r := &Runner{}
	report, err := r.RunSuite(context.Background(), "unit", tasks, m)
	if err != nil {
		t.Fatal(err)
	}
	pass, total := report.PassRate()
	if total != 2 || pass != 1 {
		t.Fatalf("pass/total = %d/%d, want 1/2", pass, total)
	}
	md := report.Markdown()
	if !strings.Contains(md, "1/2") || !strings.Contains(md, "t1") || !strings.Contains(md, "❌") {
		t.Errorf("markdown report malformed:\n%s", md)
	}
}

func TestBudgetScorer_ErrorsWhenModelNotPriceable(t *testing.T) {
	// HARNESS review L-2: a declared cost budget must not silently pass
	// when the runner cannot price the model.
	spec := &TaskSpec{Budget: BudgetSpec{MaxCostUSD: 1.0}}

	if _, err := (budgetScorer{}).Score(context.Background(), spec, &TaskOutcome{}); err == nil {
		t.Fatal("budget scorer must error when a cost budget is declared but the outcome is unpriced")
	}

	under := &TaskOutcome{CostPriced: true, CostUSD: 0.5}
	if score, err := (budgetScorer{}).Score(context.Background(), spec, under); err != nil || score != 1 {
		t.Fatalf("priced outcome under budget must pass, got score=%v err=%v", score, err)
	}
	over := &TaskOutcome{CostPriced: true, CostUSD: 2.0}
	if score, err := (budgetScorer{}).Score(context.Background(), spec, over); err != nil || score != 0 {
		t.Fatalf("priced outcome over budget must fail, got score=%v err=%v", score, err)
	}

	// No cost budget declared: unpriced outcomes pass fine.
	noBudget := &TaskSpec{Budget: BudgetSpec{MaxIters: 10}}
	if score, err := (budgetScorer{}).Score(context.Background(), noBudget, &TaskOutcome{Iters: 3}); err != nil || score != 1 {
		t.Fatalf("no cost budget must not require pricing, got score=%v err=%v", score, err)
	}
}
