package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestManagedAgentAndLoopHaveOneRetryOwner(t *testing.T) {
	for _, entry := range []string{"agent", "loop"} {
		t.Run(entry, func(t *testing.T) {
			var sends atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sends.Add(1); w.WriteHeader(503) }))
			defer server.Close()
			ledger := inference.NewLedger(100)
			controller := inference.NewController(ledger)
			defer controller.Close()
			if err := controller.RegisterPool(inference.PoolConfig{ID: "pool", MaxActive: 1}); err != nil {
				t.Fatal(err)
			}
			deployment, err := controller.Register(&inference.DeploymentConfig{Descriptor: inference.Descriptor{ID: "target", Provider: "openai", API: "openai-chat", Model: "fixture", Endpoint: server.URL}, PoolID: "pool", MaxAttempts: 2, Backoff: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			base, err := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			managed, err := model.NewManagedChatModel(base, deployment)
			if err != nil {
				t.Fatal(err)
			}
			defer managed.Close()
			a := NewUnifiedAgent("bot", "prompt", managed, WithModelConfig(ModelConfig{MaxRetries: 3, RetryDelay: time.Nanosecond}))
			ctx := inference.WithIdentity(context.Background(), inference.Identity{TenantID: "trusted"})
			if entry == "agent" {
				_, err = a.Reply(ctx, "hi")
			} else {
				_, err = loop.New(NewUnifiedAgentRunner(a).LoopOptions()...).RunSync(ctx, "hi")
			}
			if err == nil || sends.Load() != 2 {
				t.Fatalf("entry %s sent %d requests: %v", entry, sends.Load(), err)
			}
			snapshot := ledger.Snapshot()
			for _, attempt := range snapshot.Attempts {
				if attempt.Identity.AgentName != "bot" || attempt.Identity.SessionID == "" {
					t.Fatalf("agent attribution absent: %+v", attempt)
				}
			}
		})
	}
}

func TestManagedModelIdentityReachesRecoverableCostBudget(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		w.Write([]byte(`{"choices":[{"message":{"content":"done"}}],"usage":{"prompt_tokens":1000000,"completion_tokens":0}}`))
	}))
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
	base, _ := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
	m, err := model.NewManagedChatModel(base, d)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	budget := middleware.NewReplyCostBudget(0.5, middleware.WithCostBudgetPrices(map[string]model.Price{"fixture": {Input: 1}}))
	saver := newFakeStateSaver()
	a := NewUnifiedAgent("bot", "prompt", m, WithReplyRecovery(), WithStateSaver(saver), WithMiddlewares(budget, &swallowEndMiddleware{maxSwallow: 1}))
	ctx := inference.WithIdentity(context.Background(), inference.Identity{TenantID: "host"})
	if _, err := a.Reply(ctx, "hi"); err == nil {
		t.Error("exhausted cost budget did not stop continued reply")
	}
	st, err := LoadCheckpoint(context.Background(), saver, a.state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 1 || st.ReplyRecovery.Budgets.Counters[budget.Key()].SpentUSD != 1 {
		t.Fatalf("model identity or budget lost: sends=%d state=%+v", sends.Load(), st.ReplyRecovery)
	}
}

func TestManagedSummaryUsesDeploymentWindowUnlessOverridden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{map[string]any{"id": "summary", "type": "function", "function": map[string]any{"name": "generate_structured_output", "arguments": `{"task_overview":"task","current_state":"working","important_discoveries":"none","next_steps":"continue","context_to_preserve":"preferences"}`}}}}}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}})
	}))
	defer server.Close()
	c := inference.NewController(nil)
	defer c.Close()
	if err := c.RegisterPool(inference.PoolConfig{ID: "p", MaxActive: 1}); err != nil {
		t.Fatal(err)
	}
	d, err := c.Register(&inference.DeploymentConfig{Descriptor: inference.Descriptor{ID: "d", Provider: "fixture", API: "openai-chat", Model: "fixture", Endpoint: server.URL, ContextSize: 4000}, PoolID: "p", MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
	m, err := model.NewManagedChatModel(base, d)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	a := NewUnifiedAgent("bot", "prompt", m, WithContextConfig(&ContextConfig{ContextSize: 1000000}))
	for range 30 {
		a.state.Context = append(a.state.Context, message.UserMsg("user", strings.Repeat("history ", 200)))
	}
	ctx := inference.WithIdentity(context.Background(), inference.Identity{TenantID: "host"})
	if err := a.compressContext(ctx); err != nil {
		t.Fatal(err)
	}
	if len(d.Ledger().Snapshot().Attempts) != 0 {
		t.Fatal("explicit context window ignored")
	}
	a.contextCfg.ContextSize = 0
	if err := a.compressContext(ctx); err != nil {
		t.Fatal(err)
	}
	s := d.Ledger().Snapshot()
	if a.state.Summary == "" || len(s.Attempts) != 1 || s.Attempts[0].Purpose != "summary" || s.Attempts[0].Identity.AgentName != "bot" {
		t.Fatalf("summary bypassed managed path: %+v", s)
	}
}
