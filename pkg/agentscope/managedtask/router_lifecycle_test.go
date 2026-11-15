package managedtask

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
)

func TestRouterSharedClassificationGenerationAdmission(t *testing.T) {
	for _, mode := range []string{"queue-full", "queue-expired", "cancel-queued"} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			var classified atomic.Int32
			c := localJev(t, func(w http.ResponseWriter, _ *http.Request) { classified.Add(1); fmt.Fprint(w, jevRouteResponse) })
			// First task occupies the shared generation permit. The second task needs it
			// for classification, which must obey the same queue rather than bypass it.
			e := fixtureExecutor(t, func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; successFixture(w, r) }, nil)
			limits := DefaultLimits()
			limits.MaxConcurrent = 1
			limits.MaxQueued = 0
			if mode != "queue-full" {
				limits.MaxQueued = 1
			}
			limits.QueueTimeout = 30 * time.Millisecond
			router, err := NewRouter(RouterConfig{Deployments: []Config{e.cfg}, Pools: []Pool{{ID: "small", Members: []WeightedDeployment{{ID: "fixture", Weight: 1}}}}, Categories: map[string]string{"extract": "small"}, Classification: c, Limits: &limits})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first := make(chan *Result, 1)
			go func() { first <- router.Execute(ctx, evalTask("first")) }()
			await(t, entered)
			task := evalTask("classified")
			task.Category = ""
			child, stop := context.WithCancel(context.Background())
			defer stop()
			done := make(chan *Result, 1)
			go func() { done <- router.Execute(child, task) }()
			if mode == "cancel-queued" {
				waitState(t, router.deployments["fixture"], 1, 1)
				stop()
			}
			out := <-done
			if out.Status != Rejected && out.Status != Canceled {
				t.Fatalf("queue outcome: %+v", out)
			}
			if classified.Load() != 0 || len(out.Attempts) != 0 {
				t.Fatal("classifier bypassed shared capacity")
			}
			cancel()
			<-first
		})
	}
}

func TestRouterLateClassifierCleanupRetainsPermit(t *testing.T) {
	c := classificationFixture()
	base := c.Client
	entered, release := make(chan struct{}), make(chan struct{})
	c.Client = reportClassifier{DispatchClassifier: base, call: func(ctx context.Context, q classifier.Request, l classifier.DispatchLimits, begin func(classifier.AttemptKind) error) (*classifier.Response, error) {
		out, err := base.ClassifyWithDispatch(ctx, q, l, begin)
		close(entered)
		<-release
		return out, err
	}}
	router, calls := routeFixture(t, func(cfg *RouterConfig) {
		cfg.Classification = c
		l := DefaultLimits()
		l.MaxTasks = 2
		l.MaxConcurrent = 1
		l.MaxQueued = 0
		cfg.Limits = &l
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task := evalTask("late")
	task.Category = ""
	done := make(chan *Result, 1)
	go func() { done <- router.Execute(ctx, task) }()
	await(t, entered)
	cancel()
	out := <-done
	if out.Status != Canceled || len(out.Attempts) != 1 || out.Attempts[0].Completed {
		t.Fatalf("snapshot: %+v", out)
	}
	if next := router.Execute(context.Background(), evalTask("blocked")); !errors.Is(next.Err, ErrCapacity) {
		t.Fatalf("permit released early: %+v", next)
	}
	close(release)
	waitState(t, router.deployments["fixture"], 0, 0)
	if calls.Load() != 0 || out.Attempts[0].Completed || out.Attempts[0].Usage != nil {
		t.Fatal("late update or dispatch")
	}
}

func TestRouterClassificationCancellationBoundaries(t *testing.T) {
	for _, phase := range []string{"authorize", "project", "retry", "guard"} {
		t.Run(phase, func(t *testing.T) {
			c := classificationFixture()
			base := c.Client
			if phase == "authorize" {
				c.Authorize = func(context.Context, Task, ClassificationTarget) error { return context.Canceled }
			}
			if phase == "project" {
				c.Project = func(context.Context, Task, ClassificationTarget) (classifier.Request, error) {
					return classifier.Request{}, context.DeadlineExceeded
				}
			}
			var calls atomic.Int32
			if phase == "retry" {
				c = localJev(t, func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					w.Header().Set("Retry-After", "3600")
					w.WriteHeader(529)
					fmt.Fprint(w, jevRouteResponse)
				})
				c.MaxAttempts = 2
			}
			if phase == "guard" {
				var auth atomic.Int32
				c.Authorize = func(context.Context, Task, ClassificationTarget) error {
					if auth.Add(1) > 1 {
						return context.Canceled
					}
					return nil
				}
			}
			router, generation := routeFixture(t, func(cfg *RouterConfig) { cfg.Classification = c })
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			task := evalTask(phase)
			task.Category = ""
			out := router.Execute(ctx, task)
			if out.Status != Canceled && out.Status != TimedOut {
				t.Fatalf("cancel outcome: %+v", out)
			}
			if generation.Load() != 0 {
				t.Fatal("canceled classification generated")
			}
			if phase == "retry" {
				if calls.Load() != 1 || len(out.Attempts) != 1 {
					t.Fatal("cancellation started retry")
				}
			} else {
				select {
				case <-base.(*classifier.Fake).Started():
					t.Fatal("pre-dispatch cancellation sent")
				default:
				}
			}
		})
	}
}

func TestRouterClassificationInvalidProjection(t *testing.T) {
	for _, mode := range []string{"missing", "malformed", "oversized", "extra"} {
		t.Run(mode, func(t *testing.T) {
			c := classificationFixture()
			project := c.Project
			c.Project = func(ctx context.Context, task Task, target ClassificationTarget) (classifier.Request, error) {
				q, err := project(ctx, task, target)
				switch mode {
				case "missing":
					q.Questions = nil
				case "malformed":
					q.State = []byte(`{`)
				case "oversized":
					q.State = []byte(`"` + strings.Repeat("a", (1<<20)+1) + `"`)
				case "extra":
					q.Questions["extra"] = &classifier.BinaryQuestion{Instructions: "Unknown"}
				}
				return q, err
			}
			router, calls := routeFixture(t, func(cfg *RouterConfig) { cfg.Classification = c })
			task := evalTask(mode)
			task.Category = ""
			if out := router.Execute(context.Background(), task); out.Status != Rejected || calls.Load() != 0 || len(out.Attempts) != 0 {
				t.Fatalf("invalid projection: %+v", out)
			}
		})
	}
}

func TestRouterRuleContextErrorsDoNotDefault(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			c := classificationFixture()
			router, calls := routeFixture(t, func(cfg *RouterConfig) {
				cfg.Classification = c
				cfg.Default = &DefaultRoute{Category: "extract", PoolID: "small"}
				cfg.Rules = []Rule{func(context.Context, Task) (string, error) { return "", fmt.Errorf("private cause: %w", cause) }}
			})
			task := evalTask("rule-canceled")
			task.Category = ""
			result := router.Execute(context.Background(), task)
			want := Canceled
			if errors.Is(cause, context.DeadlineExceeded) {
				want = TimedOut
			}
			if result.Status != want || !errors.Is(result.Err, cause) || calls.Load() != 0 || len(result.Attempts) != 0 {
				t.Fatalf("rule context error defaulted: %+v", result)
			}
			select {
			case <-c.Client.(*classifier.Fake).Started():
				t.Fatal("canceled rule classified")
			default:
			}
		})
	}
}

func TestRouterClassifierContextErrorsAreSanitized(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			c := classificationFixture()
			base := c.Client
			c.Client = reportClassifier{DispatchClassifier: base, call: func(ctx context.Context, q classifier.Request, limits classifier.DispatchLimits, begin func(classifier.AttemptKind) error) (*classifier.Response, error) {
				out, _ := base.ClassifyWithDispatch(ctx, q, limits, begin)
				return out, fmt.Errorf("SECRET: %w", cause)
			}}
			router, calls := routeFixture(t, func(cfg *RouterConfig) {
				cfg.Classification = c
				cfg.Default = &DefaultRoute{Category: "extract", PoolID: "small"}
			})
			task := evalTask("classifier-context")
			task.Category = ""
			result := router.Execute(context.Background(), task)
			want := Canceled
			if errors.Is(cause, context.DeadlineExceeded) {
				want = TimedOut
			}
			if result.Status != want || !errors.Is(result.Err, cause) || strings.Contains(result.Err.Error(), "SECRET") || calls.Load() != 0 || len(result.Attempts) != 1 || strings.Contains(result.Attempts[0].Err.Error(), "SECRET") {
				t.Fatalf("unsafe context error: %+v", result)
			}
		})
	}
}
