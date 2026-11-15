package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/jev"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/managedtask"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/replay/evalkit"
)

func TestOfflineComparison(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), false, "", "acceptance", "test-revision", &output); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Evidence string
		Reports  []struct {
			Overall    evalkit.ManagedSummary
			Categories map[string]evalkit.ManagedSummary
		}
		Comparisons []marginComparison
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Evidence != "offline-fixtures" || len(got.Reports) != 3 || len(got.Comparisons) != 6 {
		t.Fatal("missing experiment groups")
	}
	for _, r := range got.Reports {
		if r.Overall.Planned != 12 || r.Overall.Qualified != 12 || len(r.Categories) != 3 {
			t.Fatal(r.Overall)
		}
	}
	if got.Reports[0].Overall.PhysicalRequests != 12 || got.Reports[1].Overall.AdditionalGenerationRequests != 2 || got.Reports[2].Overall.SimulatedAttempts != 6 || got.Reports[2].Overall.AdditionalGenerationRequests != 4 || got.Reports[2].Overall.CostPerQualified != nil {
		t.Fatal("missing classification/fallback/unknown-cost demonstration")
	}
	if err := run(context.Background(), true, "", "", "", io.Discard); err == nil {
		t.Fatal("live mode needs explicit plan")
	}
	if err := run(context.Background(), false, "plan.json", "acceptance", "", io.Discard); err == nil {
		t.Fatal("config silently enabled live mode")
	}
}

func TestRubricAndSplitIsolation(t *testing.T) {
	_, tuning, err := loadSamples("tuning")
	if err != nil {
		t.Fatal(err)
	}
	_, acceptance, err := loadSamples("acceptance")
	if err != nil {
		t.Fatal(err)
	}
	ids, inputs := map[string]bool{}, map[string]bool{}
	for _, s := range append(tuning, acceptance...) {
		if ids[s.ID] || inputs[s.Input] {
			t.Fatalf("split leakage: %s", s.ID)
		}
		ids[s.ID] = true
		inputs[s.Input] = true
		gold, _ := json.Marshal(s)
		spec := &evalkit.TaskSpec{Scorer: evalkit.ScorerSpec{Expect: string(gold)}}
		for _, test := range []struct {
			text string
			want float64
		}{{s.Answer, 1}, {"wrong", 0}} {
			score, e := new(rubricScorer).Score(context.Background(), spec, &evalkit.TaskOutcome{FinalText: test.text})
			if e != nil || score != test.want {
				t.Fatalf("rubric %s: %v %v", s.ID, score, e)
			}
		}
	}
	if _, _, err = loadSamples("unknown"); err == nil {
		t.Fatal("invalid split accepted")
	}
}

// Real adapters against a local Jev fixture: protocol integration, not a live
// provider test. Capture State to ensure gold rubric/category never leaves host.
func TestJevManagedProtocolFixture(t *testing.T) {
	_, samples, _ := loadSamples("acceptance")
	calls := atomic.Int32{}
	s := samples[3] // English task without a prefix, hence classifier is necessary.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "Required") || strings.Contains(string(body), "Answer") {
			t.Error("gold rubric disclosed")
		}
		var request struct {
			State string `json:"state"`
		}
		if json.Unmarshal(body, &request) != nil || request.State != s.Input {
			t.Error("wrong task projection")
		}
		fmt.Fprint(w, `{"model":"jev-fixture-v1","answers":{"category":{"type":"choice","choice":"extract","confidence":1,"probabilities":{"extract":1,"classify":0,"rewrite":0,"other":0}}},"usage":{"input_tokens":10,"output_tokens":1}}`)
	}))
	defer server.Close()
	cl, err := jev.New(jev.Config{APIKey: "local-test", Model: "jev-fixture-v1", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	generation := fixtureServer(samples, false)
	defer generation.Close()
	p := &experimentPlan{Candidates: []candidate{{ID: "baseline", BaseURL: generation.URL, Model: "baseline-fixture-v1", ContextWindow: 8192}}, DefaultID: "baseline", CategoryTargets: map[string]string{"extract": "baseline", "classify": "baseline", "rewrite": "baseline"}, Jev: classificationPlan{MinConfidence: 0.8}}
	configs, cleanup, err := buildDeployments(p, samples, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	router, err := buildRouter(p, configs, cl, samples)
	if err != nil {
		t.Fatal(err)
	}
	result := router.Execute(context.Background(), managedtask.Task{ID: "protocol", Text: s.Input, Mode: managedtask.Evaluation})
	if result.Status != managedtask.Succeeded || result.Route.Source != "classifier" || calls.Load() != 1 || len(result.Attempts) != 2 || result.Attempts[0].Kind != managedtask.PhysicalAttempt || result.Attempts[0].OperationID == "" {
		t.Fatalf("protocol integration: %+v calls=%d", result, calls.Load())
	}
	// Unqualified production must remain disabled, even for an approved task.
	production := router.Execute(context.Background(), managedtask.Task{ID: "production", Text: s.Input, Category: "extract"})
	if production.Status != managedtask.Rejected || len(production.Attempts) != 0 {
		t.Fatal("fixture evidence enabled production")
	}
}

func TestLivePlanValidation(t *testing.T) {
	version, samples, _ := loadSamples("acceptance")
	p := experimentPlan{Version: 1, SourceRevision: "test", DatasetVersion: version, RubricVersion: "managed-text-rubric-v1", Split: "acceptance", SampleSize: 12, MaxQualityLoss: 0.02, MaxP95LatencyMS: 5000, ConfirmEvaluationIsolation: true, FixedID: "baseline", DefaultID: "baseline", CategoryTargets: map[string]string{"extract": "candidate", "classify": "candidate", "rewrite": "baseline"}, Jev: classificationPlan{Endpoint: jev.DefaultEndpoint, Model: "jev-1.13.0", MinConfidence: 0.8}}
	for _, id := range []string{"baseline", "candidate"} {
		p.Candidates = append(p.Candidates, candidate{ID: id, BaseURL: "https://example.com", Model: "pinned-v1", VersionEvidence: "fixture", APIKeyEnv: "TEST_MODEL_API_KEY", ContextWindow: 8192, InputTokenUpperBound: 4096, BoundEvidence: "fixture proof"})
	}
	if err := validateLive(&p, version, samples); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*experimentPlan){func(p *experimentPlan) { p.SampleSize = 0 }, func(p *experimentPlan) { p.ConfirmEvaluationIsolation = false }, func(p *experimentPlan) { p.Jev.Endpoint = "" }, func(p *experimentPlan) { p.Jev.Model = "jev-latest" }, func(p *experimentPlan) { p.MaxP95LatencyMS = 0 }, func(p *experimentPlan) { p.Jev.MinConfidence = 0 }, func(p *experimentPlan) { p.Candidates[0].BoundEvidence = "" }, func(p *experimentPlan) { p.Candidates[0].InputTokenUpperBound = 8192 }} {
		raw, _ := json.Marshal(p)
		var test experimentPlan
		_ = json.Unmarshal(raw, &test)
		mutate(&test)
		if validateLive(&test, version, samples) == nil {
			t.Fatal("incomplete experiment accepted")
		}
	}
}

func TestLiveExperiment(t *testing.T) {
	if os.Getenv("ROUTING_LIVE_TEST") != "1" {
		t.Skip("set ROUTING_LIVE_TEST=1 and ROUTING_LIVE_CONFIG for an explicitly configured billable quality pilot")
	}
	path := os.Getenv("ROUTING_LIVE_CONFIG")
	if path == "" {
		t.Fatal("ROUTING_LIVE_CONFIG is required")
	}
	target := os.Getenv("ROUTING_LIVE_REPORT")
	if target == "" {
		t.Fatal("ROUTING_LIVE_REPORT is required to preserve experiment evidence")
	}
	var output bytes.Buffer
	if err := run(context.Background(), true, path, "", "", &output); err != nil {
		t.Fatal(err)
	}
	// Preserve the full report to a caller-specified local path. A test pass means
	// execution/reporting completed, not that quality margins or savings passed.
	if err := os.WriteFile(target, output.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("live pilot report saved; inspect all outcomes and margins before drawing conclusions")
}
