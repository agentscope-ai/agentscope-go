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
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func fixtureExecutor(t *testing.T, handler http.HandlerFunc, change func(*Config)) *Executor {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client, err := model.NewOpenAITextClient(model.OpenAIConfig{APIKey: "fixture", Model: "pinned", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	cfg := Config{Client: client, Deployment: Deployment{ID: "fixture", Endpoint: srv.URL + "/v1/chat/completions", Model: "pinned", ContextWindow: 8192, TextOnly: true, NonStreaming: true, ToolsDisabled: true}, Authorize: func(context.Context, Task, Deployment) error { return nil }, Project: func(_ context.Context, task Task, _ Deployment) (string, error) { return task.Text, nil }, InputTokenBound: func(_ context.Context, _ Deployment, wire []byte) (int, error) { return len(wire) + 32, nil }}
	if change != nil {
		change(&cfg)
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestExecuteRejectsNilContext(t *testing.T) {
	var calls, hooks atomic.Int32
	e := fixtureExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		successFixture(w, r)
	}, func(cfg *Config) {
		limits := DefaultLimits()
		limits.MaxTasks = 1
		cfg.Limits = &limits
		cfg.Authorize = func(context.Context, Task, Deployment) error {
			hooks.Add(1)
			return nil
		}
		cfg.Project = func(_ context.Context, task Task, _ Deployment) (string, error) {
			hooks.Add(1)
			return task.Text, nil
		}
		cfg.InputTokenBound = func(context.Context, Deployment, []byte) (int, error) {
			hooks.Add(1)
			return 100, nil
		}
	})
	task := evalTask("nil-context")
	r := e.Execute(nil, task) //nolint:staticcheck // exercise invalid caller input
	if r.TaskID != task.ID || r.Status != Rejected || !errors.Is(r.Err, ErrTask) || r.Text != "" || len(r.Attempts) != 0 || r.Cost != (Cost{}) {
		t.Fatalf("nil context: %+v", r)
	}
	if hooks.Load() != 0 || calls.Load() != 0 {
		t.Fatal("nil context reached host hooks or provider")
	}
	var nilExecutor *Executor
	r = nilExecutor.Execute(nil, task) //nolint:staticcheck // verify configuration error precedence
	if r.Status != Rejected || !errors.Is(r.Err, ErrConfig) || r.TaskID != task.ID {
		t.Fatalf("nil executor and context: %+v", r)
	}
	r = e.Execute(context.Background(), evalTask("valid-context"))
	if r.Status != Succeeded || calls.Load() != 1 {
		t.Fatalf("valid task after rejection: %+v", r)
	}
}

func TestExecuteProjectionAndQualification(t *testing.T) {
	var calls atomic.Int32
	e := fixtureExecutor(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"model":"pinned","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"done"}}]}`)
	}, nil)
	r := e.Execute(context.Background(), Task{ID: "1", Category: "extract", Text: "sensitive"})
	if r.Status != Rejected || calls.Load() != 0 {
		t.Fatal("unqualified production task dispatched")
	}
	r = e.Execute(context.Background(), Task{ID: "2", Category: "extract", Text: "synthetic", Mode: Evaluation})
	if r.Status != Succeeded || r.Text != "done" || len(r.Attempts) != 1 || r.Attempts[0].Kind != PhysicalAttempt || r.Cost.UnknownAttempts != 1 {
		t.Fatalf("unexpected result: %+v", r)
	}
}

const completeFixture = `{"model":"pinned","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"done"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}`

func evalTask(id string) Task {
	return Task{ID: id, Category: "extract", Text: "synthetic", Mode: Evaluation}
}
func qualify(cfg *Config) {
	d := cfg.Deployment
	cfg.Deployment.Qualifications = []Qualification{{DeploymentID: d.ID, Endpoint: d.Endpoint, Model: d.Model, Category: "extract", EvidenceID: "host-evidence-v1", DatasetVersion: "data-v1", RubricVersion: "rubric-v1", SampleSize: 20, MaxQualityLoss: 0.01, MaxLatency: time.Second}}
}
func successFixture(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, completeFixture) }
func await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture synchronization expired")
	}
}
func waitState(t *testing.T, e *Executor, active, queued int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		e.admission.mu.Lock()
		a, q := e.admission.active, len(e.admission.queue)
		e.admission.mu.Unlock()
		if a == active && q == queued {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("admission state did not converge")
}

func TestExecuteProjectionPolicyAndContext(t *testing.T) {
	var authorized atomic.Int32
	var projected atomic.Bool
	e := fixtureExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "sensitive") || !strings.Contains(string(body), "approved") {
			t.Errorf("unprojected body: %s", body)
		}
		fmt.Fprint(w, completeFixture)
	}, func(cfg *Config) {
		qualify(cfg)
		cfg.Authorize = func(_ context.Context, task Task, d Deployment) error {
			authorized.Add(1)
			if task.Text != "sensitive" || d.Model != "pinned" {
				t.Error("lost policy context")
			}
			return nil
		}
		cfg.Project = func(_ context.Context, _ Task, d Deployment) (string, error) {
			if authorized.Load() != 1 {
				t.Error("projection preceded authorization")
			}
			d.Qualifications[0].Category = "mutated"
			projected.Store(true)
			return "approved", nil
		}
		cfg.InputTokenBound = func(_ context.Context, _ Deployment, wire []byte) (int, error) {
			if !projected.Load() || strings.Contains(string(wire), "sensitive") {
				t.Error("bound did not see final projection")
			}
			wire[0] = '!'
			return 100, nil
		}
	})
	r := e.Execute(context.Background(), Task{ID: "production", Category: "extract", Text: "sensitive"})
	if r.Status != Succeeded || authorized.Load() != 2 || r.Text != "done" {
		t.Fatalf("result: %+v auth=%d", r, authorized.Load())
	}
	if e.cfg.Deployment.Qualifications[0].Category != "extract" {
		t.Fatal("mutable policy snapshot")
	}
}

func TestExecutePredispatchRejection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
		want   error
	}{
		{"denied", func(c *Config) {
			c.Authorize = func(context.Context, Task, Deployment) error { return errors.New("SECRET") }
		}, ErrDenied},
		{"projection", func(c *Config) {
			c.Project = func(context.Context, Task, Deployment) (string, error) { return "", errors.New("SECRET") }
		}, ErrProjection},
		{"empty projection", func(c *Config) {
			c.Project = func(context.Context, Task, Deployment) (string, error) { return "", nil }
		}, model.ErrTextInvalidRequest},
		{"context error", func(c *Config) {
			c.InputTokenBound = func(context.Context, Deployment, []byte) (int, error) { return 0, errors.New("SECRET") }
		}, ErrContext},
		{"negative bound", func(c *Config) {
			c.InputTokenBound = func(context.Context, Deployment, []byte) (int, error) { return -1, nil }
		}, ErrContext},
		{"too long", func(c *Config) {
			c.InputTokenBound = func(context.Context, Deployment, []byte) (int, error) { return 8192, nil }
		}, ErrContext},
		{"output reserve", func(c *Config) { c.Deployment.ContextWindow = 1 }, ErrContext},
		{"dispatch revocation", func(c *Config) {
			var n atomic.Int32
			c.Authorize = func(context.Context, Task, Deployment) error {
				if n.Add(1) > 1 {
					return errors.New("revoked")
				}
				return nil
			}
		}, ErrDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			e := fixtureExecutor(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); successFixture(w, r) }, tc.change)
			r := e.Execute(context.Background(), evalTask("1"))
			if r.Status != Rejected || !errors.Is(r.Err, tc.want) || len(r.Attempts) != 0 || calls.Load() != 0 || strings.Contains(r.Err.Error(), "SECRET") {
				t.Fatalf("rejection: %+v", r)
			}
		})
	}
}

func TestExecuteRetryAccountingAndCap(t *testing.T) {
	for _, cap := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(cap), func(t *testing.T) {
			var calls atomic.Int32
			input, output := 0.01, 0.02
			e := fixtureExecutor(t, func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if n < 3 {
					w.WriteHeader(529)
					fmt.Fprint(w, `{"model":"pinned","usage":{"prompt_tokens":5,"completion_tokens":1}}`)
					return
				}
				successFixture(w, r)
			}, func(c *Config) {
				l := DefaultLimits()
				l.MaxAttempts = cap
				l.RetryBackoff = time.Millisecond
				c.Limits = &l
				c.Prices = &Prices{Currency: "fixture", InputPerToken: &input, OutputPerToken: &output}
			})
			r := e.Execute(context.Background(), evalTask("1"))
			want := Failed
			if cap == 3 {
				want = Succeeded
			}
			if r.Status != want || len(r.Attempts) != cap || int(calls.Load()) != cap || r.Cost.UnknownAttempts != 0 {
				t.Fatalf("retry result: %+v calls=%d", r, calls.Load())
			}
			expected := float64(cap) * 0.07
			if cap == 3 {
				expected = 0.28
			}
			if math.Abs(r.Cost.KnownSubtotal-expected) > 1e-9 {
				t.Fatalf("cost=%+v expected=%f", r.Cost, expected)
			}
			for i, a := range r.Attempts {
				if a.Number != i+1 || !a.Completed || a.Usage == nil || (i < 2 && a.Err == nil) {
					t.Fatalf("lost failed attempt: %+v", a)
				}
			}
		})
	}
}

func TestExecuteResponseOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     Status
		limit      int
	}{
		{"tools", `{"model":"pinned","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"partial","tool_calls":[{}]}}],"usage":{"prompt_tokens":2}}`, Unsupported, 4096},
		{"truncated", `{"model":"pinned","choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":"partial"}}]}`, Truncated, 4096},
		{"filtered", `{"model":"pinned","choices":[{"index":0,"finish_reason":"content_filter","message":{"role":"assistant","content":"partial"}}]}`, Filtered, 4096},
		{"mismatched model", strings.ReplaceAll(completeFixture, "pinned", "other"), Failed, 4096},
		{"malformed", `{"model":"pinned","choices":[],"usage":{"prompt_tokens":2}}`, Failed, 4096},
		{"oversized", completeFixture, Failed, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			e := fixtureExecutor(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); fmt.Fprint(w, tc.body) }, func(c *Config) { l := DefaultLimits(); l.MaxResponseBytes = tc.limit; c.Limits = &l })
			r := e.Execute(context.Background(), evalTask("1"))
			if r.Status != tc.status || r.Text != "" || len(r.Attempts) != 1 || calls.Load() != 1 || !r.Attempts[0].Completed || r.Attempts[0].Err == nil {
				t.Fatalf("outcome: %+v", r)
			}
			if tc.name == "tools" || tc.name == "malformed" {
				if r.Attempts[0].Usage == nil || r.Attempts[0].Usage.InputTokens == nil {
					t.Fatal("lost failure usage")
				}
			}
		})
	}
}

func TestExecuteBoundedQueueAndTimeout(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	e := fixtureExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		select {
		case <-release:
			successFixture(w, r)
		case <-r.Context().Done():
		}
	}, func(c *Config) {
		l := DefaultLimits()
		l.MaxConcurrent = 1
		l.MaxQueued = 1
		l.MaxTasks = 4
		l.QueueTimeout = 60 * time.Millisecond
		c.Limits = &l
	})
	firstCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan *Result, 1)
	go func() { first <- e.Execute(firstCtx, evalTask("first")) }()
	await(t, entered)
	queued := make(chan *Result, 1)
	go func() { queued <- e.Execute(context.Background(), evalTask("queued")) }()
	waitState(t, e, 1, 1)
	r := e.Execute(context.Background(), evalTask("overflow"))
	if !errors.Is(r.Err, ErrCapacity) || len(r.Attempts) != 0 {
		t.Fatalf("overflow: %+v", r)
	}
	r = <-queued
	if r.Status != Rejected || !errors.Is(r.Err, ErrQueueTimeout) || len(r.Attempts) != 0 {
		t.Fatalf("queue timeout: %+v", r)
	}
	cancel()
	r = <-first
	if r.Status != Canceled || !errors.Is(r.Err, context.Canceled) || len(r.Attempts) != 1 {
		t.Fatalf("cancel: %+v", r)
	}
	waitState(t, e, 0, 0)
}

func TestExecuteRetainsCapacityUntilCallbackEnds(t *testing.T) {
	for _, phase := range []string{"projection", "dispatch"} {
		t.Run(phase, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			var calls atomic.Int32
			e := fixtureExecutor(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); successFixture(w, r) }, func(c *Config) {
				l := DefaultLimits()
				l.MaxTasks = 1
				l.MaxConcurrent = 1
				c.Limits = &l
				if phase == "projection" {
					c.Project = func(_ context.Context, task Task, _ Deployment) (string, error) {
						close(entered)
						<-release
						return task.Text, nil
					}
				} else {
					var auth atomic.Int32
					c.Authorize = func(context.Context, Task, Deployment) error {
						if auth.Add(1) == 2 {
							close(entered)
							<-release
						}
						return nil
					}
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan *Result, 1)
			go func() { done <- e.Execute(ctx, evalTask("slow")) }()
			await(t, entered)
			cancel()
			r := <-done
			if r.Status != Canceled || len(r.Attempts) != 0 {
				t.Fatalf("snapshot: %+v", r)
			}
			rejected := e.Execute(context.Background(), evalTask("second"))
			if !errors.Is(rejected.Err, ErrCapacity) {
				t.Fatal("task capacity released before callback cleanup")
			}
			if phase == "dispatch" {
				waitState(t, e, 1, 0)
			}
			close(release)
			waitState(t, e, 0, 0)
			deadline := time.Now().Add(3 * time.Second)
			for len(e.tasks) != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if len(e.tasks) != 0 || calls.Load() != 0 || r.Status != Canceled || len(r.Attempts) != 0 {
				t.Fatal("late callback dispatched or changed snapshot")
			}
		})
	}
}

func TestExecuteCancellationDuringBackoffReleasesPermit(t *testing.T) {
	var calls atomic.Int32
	e := fixtureExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(429)
			return
		}
		successFixture(w, r)
	}, func(c *Config) { l := DefaultLimits(); l.MaxConcurrent = 1; c.Limits = &l })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan *Result, 1)
	go func() { first <- e.Execute(ctx, evalTask("first")) }()
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	waitState(t, e, 0, 0)
	second := e.Execute(context.Background(), evalTask("second"))
	if second.Status != Succeeded {
		t.Fatalf("backoff held permit: %+v", second)
	}
	cancel()
	r := <-first
	if r.Status != Canceled || len(r.Attempts) != 1 || calls.Load() != 2 {
		t.Fatalf("cancel started retry: %+v calls=%d", r, calls.Load())
	}
}

func TestExecuteDeadlineAndFrozenSnapshot(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	e := fixtureExecutor(t, func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; successFixture(w, r) }, func(c *Config) { l := DefaultLimits(); l.TaskTimeout = 50 * time.Millisecond; c.Limits = &l })
	done := make(chan *Result, 1)
	go func() { done <- e.Execute(context.Background(), evalTask("deadline")) }()
	await(t, entered)
	r := <-done
	if r.Status != TimedOut || !errors.Is(r.Err, context.DeadlineExceeded) || len(r.Attempts) != 1 || r.Text != "" || r.Cost.UnknownAttempts != 1 {
		t.Fatalf("deadline: %+v", r)
	}
	copyResult := *r
	attempt := r.Attempts[0]
	waitState(t, e, 0, 0)
	if r.Status != copyResult.Status || r.Text != "" || !reflect.DeepEqual(r.Attempts[0], attempt) {
		t.Fatal("late result mutated snapshot")
	}
}

func TestExecuteCanceledBeforeStartAndInvalidTasks(t *testing.T) {
	e := fixtureExecutor(t, successFixture, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := e.Execute(ctx, evalTask("canceled"))
	if r.Status != Canceled || len(r.Attempts) != 0 || !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("pre-cancel: %+v", r)
	}
	for _, task := range []Task{{}, {ID: "id", Category: "c", Text: "x", Mode: "unknown"}, {ID: "id", Category: "c", Text: string([]byte{0xff})}, {ID: "id", Category: "c", Text: strings.Repeat("x", (1<<20)+1)}} {
		r = e.Execute(context.Background(), task)
		if r.Status != Rejected || !errors.Is(r.Err, ErrTask) {
			t.Fatalf("invalid task: %+v", r)
		}
	}
	var nilExecutor *Executor
	if nilExecutor.Execute(context.Background(), evalTask("nil")).Status != Rejected {
		t.Fatal("nil executor accepted")
	}
}
