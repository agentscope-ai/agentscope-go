package managedtask

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/jev"
	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

const jevRouteResponse = `{"model":"fixture-classifier-v1","answers":{"category":{"type":"choice","choice":"extract","confidence":0.9,"probabilities":{"extract":0.9,"other":0.1}}},"usage":{"input_tokens":5,"output_tokens":1}}`

func localJev(t *testing.T, handler http.HandlerFunc) *ClassificationConfig {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client, err := jev.New(jev.Config{APIKey: "fixture", Model: "fixture-classifier-v1", Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c := classificationFixture()
	c.Client = client
	c.Target.Endpoint = srv.URL
	return c
}

func TestRouterJevProjectionAccountingAndRetry(t *testing.T) {
	var calls, auth, project atomic.Int32
	var operations []string
	c := localJev(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "SECRET") || !strings.Contains(string(raw), "approved") {
			t.Errorf("bad projection: %s", raw)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(529)
		}
		fmt.Fprint(w, jevRouteResponse)
	})
	c.MaxAttempts = 2
	c.Authorize = func(context.Context, Task, ClassificationTarget) error { auth.Add(1); return nil }
	baseProject := c.Project
	c.Project = func(ctx context.Context, task Task, target ClassificationTarget) (classifier.Request, error) {
		if auth.Load() == 0 {
			t.Error("project before authorization")
		}
		project.Add(1)
		task.Text = "approved"
		return baseProject(ctx, task, target)
	}
	price := 0.01
	c.Prices = &Prices{Currency: "fixture", InputPerToken: &price, OutputPerToken: &price}
	r, generation := routeFixture(t, func(cfg *RouterConfig) {
		l := DefaultLimits()
		l.MaxAttempts = 3
		l.RetryBackoff = time.Millisecond
		cfg.Limits = &l
		cfg.Classification = c
		cfg.Deployments[0].Prices = &Prices{Currency: "fixture", InputPerToken: &price, OutputPerToken: &price}
	})
	task := evalTask("retry")
	task.Category = ""
	task.Text = "SECRET"
	result := r.Execute(context.Background(), task)
	if result.Status != Succeeded || calls.Load() != 2 || generation.Load() != 1 || auth.Load() != 3 || project.Load() != 1 || len(result.Attempts) != 3 {
		t.Fatalf("result: %+v calls=%d auth=%d", result, calls.Load(), auth.Load())
	}
	for i, a := range result.Attempts {
		if a.Number != i+1 || !a.Completed || a.Kind != PhysicalAttempt {
			t.Fatalf("record: %+v", a)
		}
		if i < 2 {
			operations = append(operations, a.OperationID)
		}
	}
	if operations[0] == "" || operations[0] != operations[1] || result.Attempts[0].Err == nil {
		t.Fatal("logical operation or failed record lost")
	}
	if result.Cost.UnknownAttempts != 0 || math.Abs(result.Cost.KnownSubtotal-0.24) > 1e-9 {
		t.Fatalf("cost: %+v", result.Cost)
	}
}

func TestRouterDispatchRevocationAndLimits(t *testing.T) {
	for _, phase := range []string{"revoked", "request", "response", "attempts", "project", "questions"} {
		t.Run(phase, func(t *testing.T) {
			var calls, auth atomic.Int32
			c := localJev(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); fmt.Fprint(w, jevRouteResponse) })
			c.Authorize = func(context.Context, Task, ClassificationTarget) error {
				if auth.Add(1) > 1 && phase == "revoked" {
					return errors.New("SECRET")
				}
				return nil
			}
			if phase == "project" {
				c.Project = func(context.Context, Task, ClassificationTarget) (classifier.Request, error) {
					return classifier.Request{}, errors.New("SECRET")
				}
			}
			if phase == "questions" {
				project := c.Project
				c.Project = func(ctx context.Context, task Task, target ClassificationTarget) (classifier.Request, error) {
					out, err := project(ctx, task, target)
					out.Questions["category"].(*classifier.ChoiceQuestion).Instructions = "SECRET"
					return out, err
				}
			}
			router, generation := routeFixture(t, func(cfg *RouterConfig) {
				cfg.Classification = c
				l := DefaultLimits()
				if phase == "request" {
					l.MaxRequestBytes = 160
				}
				if phase == "response" {
					l.MaxResponseBytes = 10
				}
				if phase == "attempts" {
					l.MaxAttempts = 1
				}
				cfg.Limits = &l
			})
			task := evalTask(phase)
			task.Category = ""
			out := router.Execute(context.Background(), task)
			if out.Status != Rejected || generation.Load() != 0 {
				t.Fatalf("result: %+v", out)
			}
			want := int32(0)
			if phase == "response" || phase == "attempts" {
				want = 1
			}
			if calls.Load() != want || len(out.Attempts) != int(want) {
				t.Fatalf("dispatches=%d attempts=%d", calls.Load(), len(out.Attempts))
			}
			if strings.Contains(out.Err.Error(), "SECRET") {
				t.Fatal("raw error leaked")
			}
		})
	}
}

func TestRouterFallbackChecksAndSharedBudget(t *testing.T) {
	for _, reason := range []string{"success", "denied", "unqualified", "context", "projection", "budget", "timeout"} {
		t.Run(reason, func(t *testing.T) {
			var firstCalls, secondCalls atomic.Int32
			first := fixtureExecutor(t, func(w http.ResponseWriter, _ *http.Request) {
				firstCalls.Add(1)
				w.WriteHeader(529)
				fmt.Fprint(w, `{"model":"pinned","usage":{"prompt_tokens":1,"completion_tokens":0}}`)
			}, nil)
			second := fixtureExecutor(t, func(w http.ResponseWriter, r *http.Request) { secondCalls.Add(1); successFixture(w, r) }, func(c *Config) {
				c.Deployment.ID = "second"
				if reason == "denied" {
					c.Authorize = func(context.Context, Task, Deployment) error { return ErrDenied }
				}
				if reason == "context" {
					c.Deployment.ContextWindow = 1
				}
				if reason == "projection" {
					c.Project = func(context.Context, Task, Deployment) (string, error) { return "", ErrProjection }
				}
				if reason == "timeout" {
					c.Project = func(ctx context.Context, _ Task, _ Deployment) (string, error) { <-ctx.Done(); return "", ctx.Err() }
				}
			})
			if reason == "unqualified" {
				qualify(&first.cfg)
			}
			l := DefaultLimits()
			if reason == "budget" {
				l.MaxAttempts = 1
			}
			if reason == "timeout" {
				l.TaskTimeout = 30 * time.Millisecond
			}
			router, err := NewRouter(RouterConfig{Deployments: []Config{first.cfg, second.cfg}, Pools: []Pool{{ID: "small", Members: []WeightedDeployment{{ID: "fixture", Weight: 1}}, Fallback: []string{"second"}}}, Categories: map[string]string{"extract": "small"}, Limits: &l})
			if err != nil {
				t.Fatal(err)
			}
			task := evalTask(reason)
			if reason == "unqualified" {
				task.Mode = Production
			}
			out := router.Execute(context.Background(), task)
			if firstCalls.Load() != 1 {
				t.Fatal("primary missing")
			}
			if reason == "success" {
				if out.Status != Succeeded || secondCalls.Load() != 1 || len(out.Attempts) != 2 || out.Route.DeploymentID != "second" {
					t.Fatalf("fallback: %+v", out)
				}
			} else {
				if out.Status == Succeeded || secondCalls.Load() != 0 || len(out.Attempts) != 1 {
					t.Fatalf("bypass: %+v", out)
				}
			}
		})
	}
}

func TestRouterIndependentStableWeightedTasks(t *testing.T) {
	first := fixtureExecutor(t, successFixture, nil)
	second := fixtureExecutor(t, successFixture, func(c *Config) { c.Deployment.ID = "second" })
	cfg := RouterConfig{Deployments: []Config{first.cfg, second.cfg}, Pools: []Pool{{ID: "weighted", Members: []WeightedDeployment{{ID: "fixture", Weight: 1}, {ID: "second", Weight: 3}}}, {ID: "other", Members: []WeightedDeployment{{ID: "second", Weight: 1}}}}, Categories: map[string]string{"extract": "weighted", "rewrite": "other"}}
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating host maps/slices after construction cannot alter selection.
	cfg.Categories["extract"] = "other"
	cfg.Pools[0].Members[0].ID = "missing"
	const count = 20
	out := make([]*Result, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() { defer wg.Done(); out[i] = router.Execute(context.Background(), evalTask(fmt.Sprint(i))) }()
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, result := range out {
		again := router.Execute(context.Background(), evalTask(fmt.Sprint(i)))
		if result.Status != Succeeded || result.Route.PoolID != "weighted" || result.Route != again.Route {
			t.Fatalf("unstable task: %+v %+v", result, again)
		}
		seen[result.Route.DeploymentID] = true
	}
	if len(seen) != 2 {
		t.Fatal("weights failed to exercise both deployments")
	}
	task := evalTask("other")
	task.Category = "rewrite"
	if router.Execute(context.Background(), task).Route.PoolID != "other" {
		t.Fatal("category selected wrong pool")
	}
}

func TestRouterCancellationFreezesClassificationAndRetainsCapacity(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	c := localJev(t, func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		fmt.Fprint(w, jevRouteResponse)
	})
	router, generation := routeFixture(t, func(cfg *RouterConfig) {
		cfg.Classification = c
		l := DefaultLimits()
		l.MaxConcurrent = 1
		l.MaxTasks = 1
		cfg.Limits = &l
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task := evalTask("cancel")
	task.Category = ""
	done := make(chan *Result, 1)
	go func() { done <- router.Execute(ctx, task) }()
	await(t, entered)
	cancel()
	out := <-done
	if out.Status != Canceled || len(out.Attempts) != 1 || out.Cost.UnknownAttempts != 1 {
		t.Fatalf("snapshot: %+v", out)
	}
	frozen := *out
	attempt := out.Attempts[0]
	// HTTP cancellation can finish locally before the remote handler exits.
	deadline := time.Now().Add(3 * time.Second)
	for len(router.tasks) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if generation.Load() != 0 || out.Status != frozen.Status || !reflect.DeepEqual(out.Attempts[0], attempt) {
		t.Fatal("late result changed terminal state")
	}
}

func TestRouterUncooperativeRuleRetainsTaskSlot(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	router, calls := routeFixture(t, func(cfg *RouterConfig) {
		l := DefaultLimits()
		l.MaxTasks = 1
		cfg.Limits = &l
		cfg.Rules = []Rule{func(context.Context, Task) (string, error) { close(entered); <-release; return "extract", nil }}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task := evalTask("slow")
	task.Category = ""
	done := make(chan *Result, 1)
	go func() { done <- router.Execute(ctx, task) }()
	await(t, entered)
	cancel()
	out := <-done
	if out.Status != Canceled {
		t.Fatalf("outcome: %+v", out)
	}
	if next := router.Execute(context.Background(), evalTask("second")); !errors.Is(next.Err, ErrCapacity) {
		t.Fatal("capacity released early")
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for len(router.tasks) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 0 {
		t.Fatal("canceled rule triggered generation")
	}
}

// Malformed adapter reports must not become valid routing decisions or erase a
// dispatch already committed through the guard. These adapters are local fixtures.
type reportClassifier struct {
	classifier.DispatchClassifier
	call func(context.Context, classifier.Request, classifier.DispatchLimits, func(classifier.AttemptKind) error) (*classifier.Response, error)
}

func (c reportClassifier) ClassifyWithDispatch(ctx context.Context, q classifier.Request, l classifier.DispatchLimits, begin func(classifier.AttemptKind) error) (*classifier.Response, error) {
	return c.call(ctx, q, l, begin)
}

func TestRouterInvalidClassificationReports(t *testing.T) {
	for _, failure := range []string{"nil", "operation", "kind", "negative", "no-guard", "model", "answer", "raw-error"} {
		t.Run(failure, func(t *testing.T) {
			c := classificationFixture()
			base := c.Client
			c.Client = reportClassifier{DispatchClassifier: base, call: func(ctx context.Context, q classifier.Request, l classifier.DispatchLimits, begin func(classifier.AttemptKind) error) (*classifier.Response, error) {
				if failure == "no-guard" {
					begin = nil
				}
				out, err := base.ClassifyWithDispatch(ctx, q, l, begin)
				switch failure {
				case "nil":
					out = nil
				case "operation":
					out.OperationID = "wrong"
				case "kind":
					out.Attempts[0].Kind = "bad"
				case "negative":
					v := int64(-1)
					out.Attempts[0].Usage = &classifier.Usage{InputTokens: &v}
				case "model":
					out.Model = "other"
					out.Attempts[0].Model = "other"
				case "answer":
					out.Answers["category"] = nil
				case "raw-error":
					err = errors.New("SECRET")
				}
				return out, err
			}}
			router, generation := routeFixture(t, func(cfg *RouterConfig) { cfg.Classification = c })
			task := evalTask(failure)
			task.Category = ""
			out := router.Execute(context.Background(), task)
			if out.Status != Rejected || generation.Load() != 0 || strings.Contains(out.Err.Error(), "SECRET") {
				t.Fatalf("invalid report: %+v", out)
			}
			if failure != "no-guard" && len(out.Attempts) != 1 {
				t.Fatal("lost committed dispatch")
			}
		})
	}
}

func TestRouterClassificationFailureDefaultsAndThresholds(t *testing.T) {
	for _, failure := range []string{"low-confidence", "other", "model", "contradiction", "error", "rule-error", "unknown-metadata", "unknown-rule", "absent"} {
		t.Run(failure, func(t *testing.T) {
			c := classificationFixture()
			if failure == "low-confidence" {
				c.Decisions[0].MinConfidence = 0.95
			}
			if failure == "other" {
				c.Decisions[0].Categories["extract"] = ""
			}
			if failure == "error" {
				c.Client = classifier.NewFake(&classifier.FakeConfig{Response: classifier.Response{Model: c.Target.Model}, Error: &ae.AgentError{Retryable: true}})
			}
			if failure == "model" {
				base := c.Client
				c.Client = reportClassifier{DispatchClassifier: base, call: func(ctx context.Context, q classifier.Request, l classifier.DispatchLimits, begin func(classifier.AttemptKind) error) (*classifier.Response, error) {
					out, err := base.ClassifyWithDispatch(ctx, q, l, begin)
					out.Model = "different"
					out.Attempts[0].Model = "different"
					return out, err
				}}
			}
			if failure == "contradiction" {
				d := c.Decisions[0]
				d.QuestionID = "second"
				d.Categories = map[string]string{"extract": "rewrite", "other": ""}
				c.Decisions = append(c.Decisions, d)
				original := c.Project
				c.Project = func(ctx context.Context, task Task, target ClassificationTarget) (classifier.Request, error) {
					q, err := original(ctx, task, target)
					q.Questions["second"] = &d.Question
					return q, err
				}
				c.Client = classifier.NewFake(&classifier.FakeConfig{Response: classifier.Response{Model: c.Target.Model, Answers: map[string]classifier.Answer{"category": &classifier.ChoiceAnswer{Choice: "extract", Confidence: 1, Probabilities: map[string]float64{"extract": 1, "other": 0}}, "second": &classifier.ChoiceAnswer{Choice: "extract", Confidence: 1, Probabilities: map[string]float64{"extract": 1, "other": 0}}}}})
			}
			router, generation := routeFixture(t, func(cfg *RouterConfig) {
				cfg.Categories["rewrite"] = "small"
				cfg.Classification = c
				cfg.Default = &DefaultRoute{Category: "extract", PoolID: "small"}
				if failure == "absent" {
					cfg.Classification = nil
				}
				if failure == "rule-error" {
					cfg.Rules = []Rule{func(context.Context, Task) (string, error) { return "", errors.New("SECRET") }}
				}
				if failure == "unknown-rule" {
					cfg.Rules = []Rule{func(context.Context, Task) (string, error) { return "missing", nil }}
				}
			})
			task := evalTask(failure)
			task.Category = ""
			if failure == "unknown-metadata" {
				task.Category = "missing"
			}
			out := router.Execute(context.Background(), task)
			if out.Status != Succeeded || out.Route.Source != "default" || generation.Load() != 1 {
				t.Fatalf("default route: %+v", out)
			}
		})
	}
}

func TestRouterNilInvalidAndPrecanceled(t *testing.T) {
	router, _ := routeFixture(t, nil)
	var empty *Router
	if empty.Execute(nil, Task{}).Err != ErrConfig { //nolint:staticcheck // exercise nil caller context
		t.Fatal("nil router")
	}
	if router.Execute(nil, evalTask("nil")).Err != ErrTask { //nolint:staticcheck // exercise nil caller context
		t.Fatal("nil context")
	}
	for _, task := range []Task{{}, {ID: "x", Text: "x", Mode: "bad"}, {ID: "x", Text: string([]byte{0xff})}, {ID: "x", Text: strings.Repeat("x", (1<<20)+1)}} {
		if router.Execute(context.Background(), task).Err != ErrTask {
			t.Fatal("invalid task accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out := router.Execute(ctx, evalTask("canceled")); out.Status != Canceled || len(out.Attempts) != 0 {
		t.Fatalf("pre-cancel: %+v", out)
	}
}

// Verify the projected request remains independently owned while host buffers
// and caller configuration are reused by later tasks.
func TestRouterClassificationSnapshots(t *testing.T) {
	c := classificationFixture()
	rate := 0.01
	c.Prices = &Prices{Currency: "fixture", InputPerToken: &rate, OutputPerToken: &rate}
	router, _ := routeFixture(t, func(cfg *RouterConfig) { cfg.Classification = c })
	c.Decisions[0].Question.Choices["extract"] = "mutated"
	c.Decisions[0].Categories["extract"] = ""
	rate = 100
	// Supply original policy questions from a newly constructed projection hook.
	router.classification.Project = classificationFixture().Project
	task := evalTask("snapshot")
	task.Category = ""
	if out := router.Execute(context.Background(), task); out.Status != Succeeded {
		t.Fatalf("mutable classification config: %+v", out)
	}
	if *router.classification.Prices.InputPerToken != 0.01 {
		t.Fatal("price aliases input")
	}
}
