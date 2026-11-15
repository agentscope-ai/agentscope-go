package managedtask

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestRouterConfigValidation(t *testing.T) {
	e := fixtureExecutor(t, successFixture, nil)
	fresh := func() RouterConfig {
		return RouterConfig{Deployments: []Config{e.cfg}, Pools: []Pool{{ID: "small", Members: []WeightedDeployment{{ID: "fixture", Weight: 1}}}}, Categories: map[string]string{"extract": "small"}, Classification: classificationFixture()}
	}
	changes := map[string]func(*RouterConfig){
		"no deployments":       func(c *RouterConfig) { c.Deployments = nil },
		"no pools":             func(c *RouterConfig) { c.Pools = nil },
		"no categories":        func(c *RouterConfig) { c.Categories = nil },
		"strict":               func(c *RouterConfig) { v := 0.0; c.StrictBudget = &v },
		"deployment limits":    func(c *RouterConfig) { l := DefaultLimits(); c.Deployments[0].Limits = &l },
		"bad deployment":       func(c *RouterConfig) { c.Deployments[0].Client = nil },
		"duplicate deployment": func(c *RouterConfig) { c.Deployments = append(c.Deployments, c.Deployments[0]) },
		"bad pool attempts":    func(c *RouterConfig) { c.Pools[0].AttemptsPerDeployment = -1 },
		"large pool attempts":  func(c *RouterConfig) { c.Pools[0].AttemptsPerDeployment = 3 },
		"blank pool":           func(c *RouterConfig) { c.Pools[0].ID = "" },
		"empty pool":           func(c *RouterConfig) { c.Pools[0].Members = nil },
		"duplicate pool":       func(c *RouterConfig) { c.Pools = append(c.Pools, c.Pools[0]) },
		"missing member":       func(c *RouterConfig) { c.Pools[0].Members[0].ID = "missing" },
		"zero weight":          func(c *RouterConfig) { c.Pools[0].Members[0].Weight = 0 },
		"duplicate member":     func(c *RouterConfig) { c.Pools[0].Members = append(c.Pools[0].Members, c.Pools[0].Members[0]) },
		"overflow weight": func(c *RouterConfig) {
			d := c.Deployments[0]
			d.Deployment.ID = "second"
			c.Deployments = append(c.Deployments, d)
			c.Pools[0].Members[0].Weight = math.MaxUint64
			c.Pools[0].Members = append(c.Pools[0].Members, WeightedDeployment{ID: "second", Weight: 1})
		},
		"missing fallback":             func(c *RouterConfig) { c.Pools[0].Fallback = []string{"missing"} },
		"duplicate fallback":           func(c *RouterConfig) { c.Pools[0].Fallback = []string{"fixture", "fixture"} },
		"blank category":               func(c *RouterConfig) { c.Categories[""] = "small" },
		"missing pool":                 func(c *RouterConfig) { c.Categories["extract"] = "missing" },
		"bad default":                  func(c *RouterConfig) { c.Default = &DefaultRoute{Category: "missing", PoolID: "small"} },
		"nil rule":                     func(c *RouterConfig) { c.Rules = []Rule{nil} },
		"nil classifier":               func(c *RouterConfig) { c.Classification.Client = nil },
		"nil authorize":                func(c *RouterConfig) { c.Classification.Authorize = nil },
		"nil project":                  func(c *RouterConfig) { c.Classification.Project = nil },
		"zero classification attempts": func(c *RouterConfig) { c.Classification.MaxAttempts = 0 },
		"high classification attempts": func(c *RouterConfig) { c.Classification.MaxAttempts = 3 },
		"no decisions":                 func(c *RouterConfig) { c.Classification.Decisions = nil },
		"classifier id":                func(c *RouterConfig) { c.Classification.Target.ID = "" },
		"classifier duplicate id":      func(c *RouterConfig) { c.Classification.Target.ID = "fixture" },
		"classifier endpoint":          func(c *RouterConfig) { c.Classification.Target.Endpoint = "https://wrong" },
		"classifier version":           func(c *RouterConfig) { c.Classification.Target.Model = "wrong" },
		"duplicate question": func(c *RouterConfig) {
			c.Classification.Decisions = append(c.Classification.Decisions, c.Classification.Decisions[0])
		},
		"nan threshold":      func(c *RouterConfig) { c.Classification.Decisions[0].MinConfidence = math.NaN() },
		"negative threshold": func(c *RouterConfig) { c.Classification.Decisions[0].MinConfidence = -1 },
		"high threshold":     func(c *RouterConfig) { c.Classification.Decisions[0].MinConfidence = 2 },
		"choice mapping":     func(c *RouterConfig) { delete(c.Classification.Decisions[0].Categories, "extract") },
		"missing choice mapping": func(c *RouterConfig) {
			d := &c.Classification.Decisions[0]
			delete(d.Categories, "extract")
			d.Categories["wrong"] = "extract"
		},
		"unknown category mapping": func(c *RouterConfig) { c.Classification.Decisions[0].Categories["extract"] = "missing" },
		"invalid question":         func(c *RouterConfig) { c.Classification.Decisions[0].Question.Instructions = "" },
		"invalid prices":           func(c *RouterConfig) { c.Classification.Prices = &Prices{} },
		"classifier cache prices": func(c *RouterConfig) {
			v := 0.1
			c.Classification.Prices = &Prices{Currency: "fixture", CachedInputPerToken: &v}
		},
		"negative price": func(c *RouterConfig) {
			v := -1.0
			c.Classification.Prices = &Prices{Currency: "fixture", InputPerToken: &v}
		},
		"classifier currency": func(c *RouterConfig) {
			c.Deployments[0].Prices = &Prices{Currency: "fixture"}
			c.Classification.Prices = &Prices{Currency: "other"}
		},
		"deployment currency": func(c *RouterConfig) {
			c.Deployments[0].Prices = &Prices{Currency: "fixture"}
			d := c.Deployments[0]
			d.Deployment.ID = "second"
			d.Prices = &Prices{Currency: "other"}
			c.Deployments = append(c.Deployments, d)
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			cfg := fresh()
			change(&cfg)
			if _, err := NewRouter(cfg); !errors.Is(err, ErrConfig) {
				t.Fatalf("accepted config: %v", err)
			}
		})
	}
	cfg := fresh()
	cfg.Pools[0].Fallback = []string{"fixture"}
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := router.candidates(evalTask("duplicate-fallback"), router.pools["small"]); len(got) != 1 {
		t.Fatal("selected member duplicated as fallback")
	}
}

func TestRouterUnqualifiedPoolAndFallbackQualification(t *testing.T) {
	router, calls := routeFixture(t, nil)
	task := evalTask("unqualified")
	task.Mode = Production
	if out := router.Execute(context.Background(), task); out.Status != Rejected || !errors.Is(out.Err, ErrUnqualified) || out.Route.PoolID != "small" || calls.Load() != 0 {
		t.Fatalf("result: %+v", out)
	}
}

func TestRouterCostSimulationUnknownAndOverflow(t *testing.T) {
	rate := math.MaxFloat64
	router, _ := routeFixture(t, func(c *RouterConfig) {
		c.Deployments[0].Prices = &Prices{Currency: "fixture", InputPerToken: &rate, OutputPerToken: &rate}
		c.Classification = classificationFixture()
	})
	cost := router.cost([]Attempt{{DeploymentID: "fixture", Kind: PhysicalAttempt, Model: "pinned", Completed: false}, {DeploymentID: "classifier", OperationID: "op", Kind: SimulatedAttempt}})
	if cost.UnknownAttempts != 1 || cost.KnownSubtotal != 0 {
		t.Fatalf("cost: %+v", cost)
	}
	// One finite component per attempt can still overflow the combined subtotal.
	one := int64(1)
	zero := int64(0)
	a := Attempt{DeploymentID: "fixture", Kind: PhysicalAttempt, Model: "pinned", Completed: true, Usage: &model.TextUsage{InputTokens: &one, OutputTokens: &zero}}
	cost = router.cost([]Attempt{a, a})
	if cost.UnknownAttempts != 1 || cost.KnownSubtotal != rate {
		t.Fatalf("overflow: %+v", cost)
	}
}

// Purely local no-dispatch reports cannot be used as managed classification.
func TestRouterClassifierSuccessNeedsGuard(t *testing.T) {
	c := classificationFixture()
	base := c.Client
	c.Client = reportClassifier{DispatchClassifier: base, call: func(ctx context.Context, q classifier.Request, l classifier.DispatchLimits, _ func(classifier.AttemptKind) error) (*classifier.Response, error) {
		out, err := base.ClassifyWithDispatch(ctx, q, l, nil)
		out.Attempts = nil
		return out, err
	}}
	router, _ := routeFixture(t, func(cfg *RouterConfig) { cfg.Classification = c })
	task := evalTask("no-guard")
	task.Category = ""
	if out := router.Execute(context.Background(), task); out.Status != Rejected {
		t.Fatalf("unguarded success: %+v", out)
	}
}
