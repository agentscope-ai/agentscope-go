package evalkit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestRunLoadJoinsFailedInferenceAttempts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":0}}`))
	}))
	defer server.Close()
	ledger := inference.NewLedger(100)
	controller := inference.NewController(ledger)
	defer controller.Close()
	controller.RegisterPool(inference.PoolConfig{ID: "p", MaxActive: 1})
	rate := 1.0
	d, err := controller.Register(&inference.DeploymentConfig{Descriptor: inference.Descriptor{ID: "d", Provider: "p", API: "openai-chat", Model: "fixture", Endpoint: server.URL}, PoolID: "p", MaxAttempts: 2, Backoff: time.Millisecond, Price: &inference.Price{Input: &rate}})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
	managed, err := model.NewManagedChatModel(base, d)
	if err != nil {
		t.Fatal(err)
	}
	defer managed.Close()
	manifest, cfg := loadFixture()
	cfg.AttemptLedger = ledger
	cfg.NewModel = func(ctx context.Context, _ TaskSpec) (model.ChatModel, error) {
		a := inference.AttributionFromContext(ctx)
		if a.RunID != manifest.RunID || a.Scenario != manifest.Scenario || a.Iteration != 1 || a.TaskID != "a" || a.Repeat != 1 {
			t.Errorf("missing factory attribution: %+v", a)
		}
		return managed, nil
	}
	ctx := inference.WithIdentity(context.Background(), inference.Identity{TenantID: "host"})
	report, err := (&Runner{}).RunLoad(ctx, context.Background(), manifest, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if report.Results[0].Task.Pass || report.Inference == nil || len(report.Inference.Attempts) != 2 || len(report.Results[0].InferenceAttemptIDs) != 2 || report.Inference.KnownCostUSD <= 0 {
		t.Fatalf("failed work disappeared from cost report: %+v", report)
	}
	frozen := len(report.Inference.Attempts)
	other, finish := d.Start(inference.WithAttribution(ctx, inference.Attribution{RunID: manifest.RunID, Scenario: "other"}), "embedding")
	err = inference.CurrentOperation(other).DoJSON(other, server.Client(), "POST", server.URL, struct{}{}, nil, nil)
	finish(err)
	if len(report.Inference.Attempts) != frozen {
		t.Fatal("published snapshot changed")
	}
}
func TestRunLoadMarksUnmanagedAccountingIncomplete(t *testing.T) {
	manifest, cfg := loadFixture()
	cfg.AttemptLedger = inference.NewLedger(10)
	r, err := (&Runner{}).RunLoad(context.Background(), context.Background(), manifest, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.Inference == nil || !r.Inference.Incomplete {
		t.Fatal("uninstrumented task reported complete zero cost")
	}
}

func TestRunLoadJoinsScoringWithIndependentIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"done"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer server.Close()
	ledger := inference.NewLedger(20)
	c := inference.NewController(ledger)
	defer c.Close()
	if err := c.RegisterPool(inference.PoolConfig{ID: "p", MaxActive: 1}); err != nil {
		t.Fatal(err)
	}
	d, err := c.Register(&inference.DeploymentConfig{Descriptor: inference.Descriptor{ID: "d", Provider: "fixture", API: "openai-chat", Model: "fixture", Endpoint: server.URL}, PoolID: "p", MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
	managed, err := model.NewManagedChatModel(base, d)
	if err != nil {
		t.Fatal(err)
	}
	defer managed.Close()
	m, cfg := loadFixture()
	cfg.AttemptLedger = ledger
	cfg.NewModel = func(context.Context, TaskSpec) (model.ChatModel, error) { return managed, nil }
	cfg.Scorer = loadScorerFunc(func(ctx context.Context, _ *TaskSpec, _ *TaskOutcome) (float64, error) {
		_, err := managed.Chat(ctx, []*message.Msg{message.UserMsg("scorer", "grade")})
		return 1, err
	})
	execCtx := inference.WithIdentity(context.Background(), inference.Identity{TenantID: "execution"})
	scoreCtx := inference.WithIdentity(context.Background(), inference.Identity{TenantID: "scoring"})
	r, err := (&Runner{}).RunLoad(execCtx, scoreCtx, m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Results[0].Task.Pass || len(r.Results[0].InferenceAttemptIDs) != 2 {
		t.Fatalf("score attempt missing: %+v", r)
	}
	a := r.Inference.Attempts[1]
	if a.Purpose != "scoring" || a.Identity.TenantID != "scoring" || a.Attribution.Iteration != 1 || a.Attribution.Repeat != 1 {
		t.Fatalf("score inherited wrong attribution: %+v", a)
	}
}

func TestInferenceJoinRejectsUnmatchedAttribution(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{}`)) }))
	defer server.Close()
	c := inference.NewController(nil)
	defer c.Close()
	if err := c.RegisterPool(inference.PoolConfig{ID: "p", MaxActive: 1}); err != nil {
		t.Fatal(err)
	}
	d, err := c.Register(&inference.DeploymentConfig{Descriptor: inference.Descriptor{ID: "d", Provider: "fixture", API: "openai-chat", Model: "fixture", Endpoint: server.URL}, PoolID: "p", MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := loadFixture()
	for _, a := range []inference.Attribution{
		{RunID: m.RunID, Scenario: m.Scenario, Iteration: 0},
		{RunID: m.RunID, Scenario: m.Scenario, Iteration: 1, TaskID: "wrong", Repeat: 1},
	} {
		ctx := inference.WithAttribution(inference.WithIdentity(context.Background(), inference.Identity{TenantID: "host"}), a)
		ctx, finish := d.Start(ctx, "scoring")
		err := inference.CurrentOperation(ctx).DoJSON(ctx, server.Client(), "POST", server.URL, struct{}{}, nil, nil)
		finish(err)
		if err != nil {
			t.Fatal(err)
		}
	}
	report := &LoadReport{Manifest: *m, Results: make([]TaskQualityResult, 1)}
	joinInference(report, d.Ledger(), nil)
	if report.InferenceUnmatched != 2 || !report.Inference.Incomplete || len(report.Results[0].InferenceAttemptIDs) != 0 {
		t.Fatalf("unmatched attempts silently assigned: %+v", report)
	}
}

func TestFailedModelFactoryKeepsAccountingUnverified(t *testing.T) {
	m, cfg := loadFixture()
	cfg.AttemptLedger = inference.NewLedger(10)
	cfg.NewModel = func(context.Context, TaskSpec) (model.ChatModel, error) {
		// A failed factory may already have made uninstrumented warmup calls.
		return nil, errors.New("setup failed after warmup")
	}
	r, err := (&Runner{}).RunLoad(context.Background(), context.Background(), m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Inference.Incomplete || !r.Results[0].InferenceUnmanaged {
		t.Fatal("failed factory claimed verified zero usage")
	}
}
