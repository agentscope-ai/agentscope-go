package managedtask

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
)

func routeFixture(t *testing.T, change func(*RouterConfig)) (*Router, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	e := fixtureExecutor(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); successFixture(w, r) }, nil)
	cfg := RouterConfig{Deployments: []Config{e.cfg}, Pools: []Pool{{ID: "small", Members: []WeightedDeployment{{ID: "fixture", Weight: 1}}}}, Categories: map[string]string{"extract": "small"}}
	if change != nil {
		change(&cfg)
	}
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return router, calls
}

func classificationFixture() *ClassificationConfig {
	q := classifier.ChoiceQuestion{Instructions: "Classify permitted text", Choices: map[string]string{"extract": "Extraction", "other": "Unknown"}}
	fake := classifier.NewFake(&classifier.FakeConfig{Response: classifier.Response{Model: "fixture-classifier-v1", Answers: map[string]classifier.Answer{"category": &classifier.ChoiceAnswer{Choice: "extract", Confidence: 0.9, Probabilities: map[string]float64{"extract": 0.9, "other": 0.1}}}}})
	endpoint, name := fake.Target()
	return &ClassificationConfig{Client: fake, Target: ClassificationTarget{ID: "classifier", Endpoint: endpoint, Model: name}, MaxAttempts: 1,
		Authorize: func(context.Context, Task, ClassificationTarget) error { return nil },
		Project: func(_ context.Context, task Task, _ ClassificationTarget) (classifier.Request, error) {
			b, _ := json.Marshal(task.Text)
			return classifier.Request{State: b, Questions: map[string]classifier.Question{"category": &q}}, nil
		},
		Decisions: []ChoiceDecision{{QuestionID: "category", Question: q, MinConfidence: 0.8, Categories: map[string]string{"extract": "extract", "other": ""}}}}
}

func TestRouterMetadataRulesAndClassification(t *testing.T) {
	for _, source := range []string{"metadata", "rule", "classifier"} {
		t.Run(source, func(t *testing.T) {
			c := classificationFixture()
			var rules atomic.Int32
			router, calls := routeFixture(t, func(cfg *RouterConfig) {
				cfg.Classification = c
				cfg.Rules = []Rule{func(context.Context, Task) (string, error) {
					rules.Add(1)
					if source == "rule" {
						return "extract", nil
					}
					return "", nil
				}}
			})
			task := evalTask(source)
			if source != "metadata" {
				task.Category = ""
			}
			result := router.Execute(context.Background(), task)
			if result.Status != Succeeded || result.Route.Source != source || result.Route.PoolID != "small" || calls.Load() != 1 {
				t.Fatalf("result: %+v", result)
			}
			want := 1
			if source == "classifier" {
				want = 2
			}
			if len(result.Attempts) != want {
				t.Fatalf("attempts: %+v", result.Attempts)
			}
			select {
			case <-c.Client.(*classifier.Fake).Started():
				if source != "classifier" {
					t.Fatal("unnecessary classification")
				}
			default:
				if source == "classifier" {
					t.Fatal("missing classification")
				}
			}
			if source == "metadata" && rules.Load() != 0 {
				t.Fatal("metadata did not take precedence")
			}
		})
	}
}

func TestRouterDeniedClassifierAndExplicitDefault(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "reject", true: "default"}[fallback], func(t *testing.T) {
			c := classificationFixture()
			var projections atomic.Int32
			c.Authorize = func(context.Context, Task, ClassificationTarget) error { return errors.New("SECRET") }
			c.Project = func(context.Context, Task, ClassificationTarget) (classifier.Request, error) {
				projections.Add(1)
				return classifier.Request{}, nil
			}
			router, calls := routeFixture(t, func(cfg *RouterConfig) {
				cfg.Classification = c
				if fallback {
					cfg.Default = &DefaultRoute{Category: "extract", PoolID: "small"}
				}
			})
			task := evalTask("denied")
			task.Category = ""
			r := router.Execute(context.Background(), task)
			if projections.Load() != 0 {
				t.Fatal("constructed forbidden request")
			}
			select {
			case <-c.Client.(*classifier.Fake).Started():
				t.Fatal("forbidden classifier sent")
			default:
			}
			if fallback {
				if r.Status != Succeeded || calls.Load() != 1 || len(r.Attempts) != 1 || r.Route.Source != "default" {
					t.Fatalf("default: %+v", r)
				}
			} else if r.Status != Rejected || !errors.Is(r.Err, ErrRoute) || calls.Load() != 0 || len(r.Attempts) != 0 {
				t.Fatalf("rejection: %+v", r)
			}
		})
	}
}
