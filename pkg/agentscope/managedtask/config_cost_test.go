package managedtask

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestConfigValidationAndSnapshots(t *testing.T) {
	e := fixtureExecutor(t, successFixture, qualify)
	base := e.cfg
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"client", func(c *Config) { c.Client = nil }},
		{"authorize", func(c *Config) { c.Authorize = nil }},
		{"project", func(c *Config) { c.Project = nil }},
		{"bound", func(c *Config) { c.InputTokenBound = nil }},
		{"strict budget", func(c *Config) { n := 0.0; c.StrictBudget = &n }},
		{"id", func(c *Config) { c.Deployment.ID = "" }},
		{"endpoint", func(c *Config) { c.Deployment.Endpoint = "https://other/v1/chat/completions" }},
		{"model", func(c *Config) { c.Deployment.Model = "alias" }},
		{"context", func(c *Config) { c.Deployment.ContextWindow = 0 }},
		{"capabilities", func(c *Config) { c.Deployment.ToolsDisabled = false }},
		{"limits", func(c *Config) { l := DefaultLimits(); l.MaxTasks = 0; c.Limits = &l }},
		{"queue", func(c *Config) { l := DefaultLimits(); l.MaxQueued = -1; c.Limits = &l }},
		{"response size overflow", func(c *Config) { l := DefaultLimits(); l.MaxResponseBytes = math.MaxInt; c.Limits = &l }},
		{"qualification binding", func(c *Config) { c.Deployment.Qualifications[0].Endpoint = "https://other" }},
		{"qualification evidence", func(c *Config) { c.Deployment.Qualifications[0].EvidenceID = "" }},
		{"qualification margin", func(c *Config) { c.Deployment.Qualifications[0].MaxQualityLoss = math.NaN() }},
		{"currency", func(c *Config) { c.Prices = &Prices{} }},
		{"invalid price", func(c *Config) { n := math.Inf(1); c.Prices = &Prices{Currency: "fixture", InputPerToken: &n} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			c.Deployment = e.deployment()
			tc.change(&c)
			if _, err := New(c); !errors.Is(err, ErrConfig) {
				t.Fatalf("invalid config accepted: %v", err)
			}
		})
	}
	limits := DefaultLimits()
	rate := 0.01
	c := base
	c.Deployment = e.deployment()
	c.Limits = &limits
	c.Prices = &Prices{Currency: "fixture", InputPerToken: &rate, OutputPerToken: &rate}
	snapshot, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	limits.MaxTasks = 0
	rate = 100
	c.Prices.Currency = "mutated"
	c.Deployment.Qualifications[0].Category = "mutated"
	r := snapshot.Execute(context.Background(), Task{ID: "1", Category: "extract", Text: "synthetic"})
	if r.Status != Succeeded || r.Cost.Currency != "fixture" || math.Abs(r.Cost.KnownSubtotal-0.12) > 1e-9 {
		t.Fatalf("mutable config: %+v", r)
	}
	*r.Attempts[0].Usage.InputTokens = 999
	r2 := snapshot.Execute(context.Background(), Task{ID: "2", Category: "extract", Text: "synthetic"})
	if *r2.Attempts[0].Usage.InputTokens != 10 {
		t.Fatal("result records alias calls")
	}
}

func TestCostKnownSubtotalAndUnknownComponents(t *testing.T) {
	input, output, cached := int64(10), int64(2), int64(4)
	inRate, outRate, cacheRate := 0.01, 0.02, 0.005
	e := fixtureExecutor(t, successFixture, nil)
	e.cfg.Prices = &Prices{Currency: "fixture", InputPerToken: &inRate, OutputPerToken: &outRate, CachedInputPerToken: &cacheRate}
	a := Attempt{Completed: true, Model: "pinned", Usage: &model.TextUsage{InputTokens: &input, OutputTokens: &output, CachedInputTokens: &cached}}
	cost := e.cost([]Attempt{a})
	if cost.UnknownAttempts != 0 || math.Abs(cost.KnownSubtotal-0.12) > 1e-9 {
		t.Fatalf("cached cost: %+v", cost)
	}
	a.Usage.CachedInputTokens = nil
	cost = e.cost([]Attempt{a})
	if cost.UnknownAttempts != 1 || math.Abs(cost.KnownSubtotal-0.04) > 1e-9 {
		t.Fatalf("unknown cache: %+v", cost)
	}
	e.cfg.Prices.CachedInputPerToken = nil
	e.cfg.Prices.InputPerToken = nil
	cost = e.cost([]Attempt{a})
	if cost.UnknownAttempts != 1 || math.Abs(cost.KnownSubtotal-0.04) > 1e-9 {
		t.Fatalf("unknown input price: %+v", cost)
	}
	a.Completed = false
	cost = e.cost([]Attempt{a})
	if cost.UnknownAttempts != 1 || cost.KnownSubtotal != 0 {
		t.Fatal("unfinished cost claimed known")
	}
	a.Completed = true
	a.Model = "other"
	cost = e.cost([]Attempt{a})
	if cost.UnknownAttempts != 1 || cost.KnownSubtotal != 0 {
		t.Fatal("price applied to other model")
	}
	a.Model = "pinned"
	a.Usage = nil
	cost = e.cost([]Attempt{a})
	if cost.UnknownAttempts != 1 {
		t.Fatal("unknown usage counted free")
	}
	huge := math.MaxFloat64
	e.cfg.Prices.OutputPerToken = &huge
	a.Usage = &model.TextUsage{OutputTokens: &output}
	cost = e.cost([]Attempt{a})
	if !finite(cost.KnownSubtotal) || cost.UnknownAttempts != 1 {
		t.Fatal("infinite cost")
	}
}

func TestHostContextErrorPrecedence(t *testing.T) {
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		e := fixtureExecutor(t, successFixture, func(c *Config) {
			c.Project = func(context.Context, Task, Deployment) (string, error) { return "", fmt.Errorf("SECRET: %w", sentinel) }
		})
		r := e.Execute(context.Background(), evalTask("1"))
		want := Canceled
		if sentinel == context.DeadlineExceeded {
			want = TimedOut
		}
		if r.Status != want || !errors.Is(r.Err, sentinel) || len(r.Attempts) != 0 {
			t.Fatalf("context cause: %+v", r)
		}
	}
	// A task deadline that expires inside a sizing callback wins over its raw error.
	e := fixtureExecutor(t, successFixture, func(c *Config) {
		l := DefaultLimits()
		l.TaskTimeout = 20 * time.Millisecond
		c.Limits = &l
		c.InputTokenBound = func(ctx context.Context, _ Deployment, _ []byte) (int, error) {
			<-ctx.Done()
			return 0, errors.New("SECRET")
		}
	})
	r := e.Execute(context.Background(), evalTask("1"))
	if r.Status != TimedOut || !errors.Is(r.Err, context.DeadlineExceeded) {
		t.Fatalf("deadline cause: %+v", r)
	}
}
