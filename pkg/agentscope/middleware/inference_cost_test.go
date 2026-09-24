package middleware

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestCostLedgerProjectsAttemptsWithoutLogicalDoubleCount(t *testing.T) {
	source := inference.NewLedger(10)
	controller := inference.NewController(source)
	defer controller.Close()
	controller.RegisterPool(inference.PoolConfig{ID: "p", MaxActive: 1})
	rate := 1.0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":0}}`))
	}))
	defer server.Close()
	d, err := controller.Register(&inference.DeploymentConfig{Descriptor: inference.Descriptor{ID: "d", Provider: "p", API: "openai-chat", Model: "fixture", Endpoint: server.URL}, PoolID: "p", MaxAttempts: 1, Price: &inference.Price{Input: &rate}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, finish := d.Start(inference.WithIdentity(context.Background(), inference.Identity{TenantID: "t", SessionID: "s", AgentName: "a"}), "reasoning")
	err = inference.CurrentOperation(ctx).DoJSON(ctx, server.Client(), "POST", server.URL, struct{}{}, nil, nil)
	finish(err)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := NewCostLedgerFromAttempts(source)
	if err != nil {
		t.Fatal(err)
	}
	tracking := NewCostTracking(ledger, "s", "a", map[string]model.Price{"fixture": {Input: 1}})
	tracking.OnModelCall(context.Background(), &ModelCallInput{ModelName: "fixture"}, func(context.Context, *ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{Usage: &model.ChatUsage{InputTokens: 100}}, nil
	})
	summary := ledger.Summary(CostFilter{SessionID: "s"})
	if summary.Calls != 1 || summary.TotalInTokens != 100 || summary.UnknownUsage != 0 || summary.UnknownCost != 0 || math.Abs(summary.TotalCostUSD-0.0001) > 1e-15 {
		t.Fatalf("physical facts double-counted or lost: %+v", summary)
	}
	if _, err := NewCostLedgerFromAttempts(nil); err == nil {
		t.Fatal("nil source accepted")
	}
}

func TestCostProjectionIncludesPendingOperations(t *testing.T) {
	source := inference.NewLedger(10)
	c := inference.NewController(source)
	defer c.Close()
	if err := c.RegisterPool(inference.PoolConfig{ID: "p", MaxActive: 1}); err != nil {
		t.Fatal(err)
	}
	d, err := c.Register(&inference.DeploymentConfig{Descriptor: inference.Descriptor{ID: "d", Provider: "fixture", API: "openai-chat", Model: "fixture", Endpoint: "http://localhost"}, PoolID: "p", MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	projection, err := NewCostLedgerFromAttempts(source)
	if err != nil {
		t.Fatal(err)
	}
	_, finish := d.Start(inference.WithIdentity(context.Background(), inference.Identity{TenantID: "host", SessionID: "waiting", AgentName: "bot"}), "reasoning")
	defer finish(context.Canceled)
	for _, filter := range []CostFilter{{SessionID: "waiting"}, {AgentName: "bot"}, {ModelName: "fixture"}} {
		if s := projection.Summary(filter); !s.Incomplete || s.Calls != 0 {
			t.Fatalf("pending demand reported settled: %+v", s)
		}
	}
	for _, filter := range []CostFilter{{SessionID: "other"}, {AgentName: "other"}, {ModelName: "other"}} {
		if s := projection.Summary(filter); s.Incomplete {
			t.Fatalf("unrelated operation contaminated filtered view: %+v", s)
		}
	}
	finish(context.Canceled)
	if s := projection.Summary(CostFilter{}); s.Incomplete || s.Calls != 0 {
		t.Fatalf("completed pre-send failure reported pending: %+v", s)
	}
}
