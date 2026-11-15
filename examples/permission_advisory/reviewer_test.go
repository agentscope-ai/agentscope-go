package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type testClassifier struct {
	calls           atomic.Int32
	mu              sync.Mutex
	endpoint, model string
	act             func(context.Context, classifier.Request, classifier.DispatchLimits, func(classifier.AttemptKind) error) (*classifier.Response, error)
}

func (c *testClassifier) Target() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.endpoint == "" {
		return "fixture://classifier", "fixture-model"
	}
	return c.endpoint, c.model
}
func (c *testClassifier) Classify(ctx context.Context, req classifier.Request) (*classifier.Response, error) {
	return c.ClassifyWithDispatch(ctx, req, classifier.DispatchLimits{}, func(classifier.AttemptKind) error { return nil })
}
func (c *testClassifier) ClassifyWithDispatch(ctx context.Context, req classifier.Request, limits classifier.DispatchLimits, begin func(classifier.AttemptKind) error) (*classifier.Response, error) {
	c.calls.Add(1)
	if c.act != nil {
		return c.act(ctx, req, limits, begin)
	}
	return validReply(ctx, req, begin)
}
func validReply(_ context.Context, req classifier.Request, begin func(classifier.AttemptKind) error) (*classifier.Response, error) {
	out := &classifier.Response{OperationID: req.OperationID}
	if err := begin(classifier.SimulatedAttempt); err != nil {
		return out, err
	}
	out.Model = "fixture-model"
	out.Attempts = []classifier.Attempt{{Kind: classifier.SimulatedAttempt, Model: "fixture-model"}}
	out.Answers = map[string]classifier.Answer{"review": &classifier.ChoiceAnswer{Choice: "allow_once", Confidence: 0.8, Probabilities: map[string]float64{"allow_once": 0.9, "needs_human": 0.1}}}
	return out, nil
}
func fixtureReviewer(t *testing.T, c classifier.DispatchClassifier) (*reviewer, *middleware.CheckPermissionInput, context.Context, *bytes.Buffer) {
	t.Helper()
	target := newSyntheticTool()
	p := newPolicy(target)
	p.engine = permission.NewEngine(permission.NewContext(permission.ModeDefault))
	var output bytes.Buffer
	r, err := newReviewer(c, p, &output, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	scope := &replyScope{id: 1, intentOK: true, seen: make(map[string]bool)}
	ctx := context.WithValue(context.Background(), scopeKey{}, scope)
	in := &middleware.CheckPermissionInput{AgentName: "demo", ToolCall: message.ToolCallBlock{Type: "tool_call", ID: "call", Name: "read_fixture", Input: `{"target":"fixture.csv"}`}, ToolInput: map[string]any{"target": "fixture.csv"}}
	return r, in, ctx, &output
}
func originalAsk(r *reviewer, in *middleware.CheckPermissionInput) *permission.Decision {
	d, _ := r.policy.engine.CheckPermission(r.policy.checker, in.ToolInput)
	return &d
}
func nextResult(d *permission.Decision) middleware.CheckPermissionHandler {
	return func(context.Context, *middleware.CheckPermissionInput) (*permission.Decision, error) { return d, nil }
}
func records(t *testing.T, output *bytes.Buffer) []auditRecord {
	t.Helper()
	var got []auditRecord
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	for {
		var rec auditRecord
		err := decoder.Decode(&rec)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, rec)
	}
	return got
}
func TestAdviceNeverChangesPermission(t *testing.T) {
	c := &testClassifier{}
	r, in, ctx, output := fixtureReviewer(t, c)
	d := originalAsk(r, in)
	d.Message = "secret-message"
	d.DecisionReason = "sensitive-reason"
	d.UpdatedInput = map[string]any{"secret": "must-not-log"}
	before := *d
	got, err := r.OnCheckPermission(ctx, in, nextResult(d))
	if err != nil || got != d || !reflect.DeepEqual(*got, before) {
		t.Fatal("advice changed original decision")
	}
	rs := records(t, output)
	if c.calls.Load() != 1 || len(rs) != 1 || rs[0].Recommendation != "allow_once" || rs[0].Final != permission.BehaviorAsk {
		t.Fatalf("unexpected advice: %s", output.String())
	}
	for _, secret := range []string{"secret-message", "sensitive-reason", "must-not-log", "fixture.csv", "fixture://classifier"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("audit leaked %q", secret)
		}
	}
}
func TestIneligibleNeverDispatches(t *testing.T) {
	for _, kind := range []string{"unset", "deny", "explicit ask", "safety ask", "allow", "unknown origin", "unknown future origin", "non-default", "projection extra", "projection target", "uncontrolled intent", "external backend", "backend working directory", "no scope", "next error", "nil result"} {
		t.Run(kind, func(t *testing.T) {
			c := &testClassifier{}
			r, in, ctx, output := fixtureReviewer(t, c)
			d := originalAsk(r, in)
			next := nextResult(d)
			switch kind {
			case "unset":
				r.client = nil
			case "deny":
				d.Behavior = permission.BehaviorDeny
			case "explicit ask":
				d.Origin = permission.OriginUnknown
			case "safety ask":
				d.BypassImmune = true
			case "allow":
				d.Behavior = permission.BehaviorAllow
			case "unknown origin":
				d.Origin = permission.OriginUnknown
			case "unknown future origin":
				d.Origin = "future"
			case "non-default":
				r.policy.engine = permission.NewEngine(permission.NewContext(permission.ModeAcceptEdits))
			case "projection extra":
				in.ToolInput["token"] = "secret"
			case "projection target":
				in.ToolInput["target"] = "secret-file"
			case "uncontrolled intent":
				ctx.Value(scopeKey{}).(*replyScope).intentOK = false
			case "external backend":
				ctx = tool.WithBackend(ctx, &foreignBackend{})
			case "backend working directory":
				ctx = tool.WithBackend(ctx, &tool.LocalBackend{WorkDir: "other-root"})
			case "no scope":
				ctx = context.Background()
			case "next error":
				next = func(context.Context, *middleware.CheckPermissionInput) (*permission.Decision, error) {
					return nil, errors.New("core failed")
				}
			case "nil result":
				next = nextResult(nil)
			}
			got, err := r.OnCheckPermission(ctx, in, next)
			if kind == "next error" {
				if err == nil {
					t.Fatal("lost engine error")
				}
			} else if kind == "nil result" {
				if got != nil {
					t.Fatal("nil core changed")
				}
			} else if err != nil || got != d {
				t.Fatal("changed deterministic outcome")
			}
			if c.calls.Load() != 0 {
				t.Fatal("ineligible classifier dispatch")
			}
			if kind == "unset" && output.Len() != 0 {
				t.Fatal("unset feature emitted audit")
			}
		})
	}
}
func TestReviewFailuresRemainAsk(t *testing.T) {
	for _, kind := range []string{"timeout", "parent cancel", "provider error", "wrong model", "bad response", "invalid report", "no guard", "target changed", "policy changed", "scope closed"} {
		t.Run(kind, func(t *testing.T) {
			c := &testClassifier{}
			r, in, base, output := fixtureReviewer(t, c)
			ctx, cancel := context.WithCancel(base)
			defer cancel()
			d := originalAsk(r, in)
			r.timeout = 10 * time.Millisecond
			c.act = func(callCtx context.Context, req classifier.Request, _ classifier.DispatchLimits, begin func(classifier.AttemptKind) error) (*classifier.Response, error) {
				if kind == "timeout" {
					<-callCtx.Done()
					return &classifier.Response{OperationID: req.OperationID}, callCtx.Err()
				}
				if kind == "parent cancel" {
					cancel()
					return &classifier.Response{OperationID: req.OperationID}, ctx.Err()
				}
				if kind == "no guard" {
					return validReply(callCtx, req, func(classifier.AttemptKind) error { return nil })
				}
				out, err := validReply(callCtx, req, begin)
				if err != nil {
					return out, err
				}
				switch kind {
				case "provider error":
					return out, errors.New("secret-provider-error")
				case "wrong model":
					out.Model = "secret-actual-model"
				case "bad response":
					out.Answers = nil
				case "invalid report":
					out.OperationID = "mismatch"
				case "target changed":
					c.mu.Lock()
					c.endpoint, c.model = "secret-target", "other-model"
					c.mu.Unlock()
				case "policy changed":
					r.policy.addRule(permission.Rule{ToolName: "other", Behavior: permission.BehaviorDeny})
				case "scope closed":
					s := ctx.Value(scopeKey{}).(*replyScope)
					s.mu.Lock()
					s.closed = true
					s.mu.Unlock()
				}
				return out, nil
			}
			got, err := r.OnCheckPermission(ctx, in, nextResult(d))
			if err != nil || got != d || d.Behavior != permission.BehaviorAsk {
				t.Fatal("review failure changed Ask")
			}
			for _, rec := range records(t, output) {
				if rec.Status == "advice" || rec.Recommendation != "" {
					t.Fatalf("failed review emitted advice: %s", output.String())
				}
			}
			if strings.Contains(output.String(), "secret-") {
				t.Fatal("untrusted failure details leaked")
			}
		})
	}
}
func TestBoundedScopesAndDispatchChecks(t *testing.T) {
	for _, kind := range []string{"duplicate", "full", "busy", "closed", "stale before dispatch"} {
		t.Run(kind, func(t *testing.T) {
			c := &testClassifier{}
			r, in, ctx, output := fixtureReviewer(t, c)
			d := originalAsk(r, in)
			s := ctx.Value(scopeKey{}).(*replyScope)
			switch kind {
			case "duplicate":
				_, _ = r.OnCheckPermission(ctx, in, nextResult(d))
				output.Reset()
			case "full":
				for i := 0; i < reviewLimit; i++ {
					s.seen[fmt.Sprint(i)] = true
				}
			case "busy":
				r.slots <- struct{}{}
				defer func() { <-r.slots }()
			case "closed":
				s.closed = true
			case "stale before dispatch":
				c.act = func(c context.Context, req classifier.Request, _ classifier.DispatchLimits, b func(classifier.AttemptKind) error) (*classifier.Response, error) {
					r.policy.addRule(permission.Rule{ToolName: "other", Behavior: permission.BehaviorDeny})
					return validReply(c, req, b)
				}
			}
			before := c.calls.Load()
			got, err := r.OnCheckPermission(ctx, in, nextResult(d))
			if err != nil || got != d {
				t.Fatal("scope failure changed Ask")
			}
			if kind != "stale before dispatch" && c.calls.Load() != before {
				t.Fatal("scope bound bypassed")
			}
			for _, rec := range records(t, output) {
				if rec.Status == "advice" {
					t.Fatal("invalid scope issued advice")
				}
			}
		})
	}
}
func TestDispatchInvalidation(t *testing.T) {
	for _, kind := range []string{"unchanged", "expired", "arguments", "call ID", "policy", "environment", "backend context", "classifier", "cross reply", "replay"} {
		t.Run(kind, func(t *testing.T) {
			c := &testClassifier{}
			r, in, ctx, output := fixtureReviewer(t, c)
			d := originalAsk(r, in)
			if _, err := r.OnCheckPermission(ctx, in, nextResult(d)); err != nil {
				t.Fatal(err)
			}
			output.Reset()
			scope := ctx.Value(scopeKey{}).(*replyScope)
			switch kind {
			case "expired":
				scope.pending.expires = r.now().Add(-time.Second)
			case "arguments":
				in.ToolCall.Input = `{"target":"other.csv"}`
			case "call ID":
				in.ToolCall.ID = "changed"
			case "policy":
				r.policy.addRule(permission.Rule{ToolName: "read_fixture", Behavior: permission.BehaviorAsk})
			case "environment":
				r.policy.changeEnvironment("new", "new")
			case "backend context":
				ctx = tool.WithBackend(ctx, &tool.LocalBackend{WorkDir: "other-root"})
			case "classifier":
				c.endpoint, c.model = "changed", "changed"
			case "cross reply":
				ctx = context.WithValue(ctx, scopeKey{}, &replyScope{id: 2, intentOK: true, seen: make(map[string]bool)})
			}
			executed := 0
			next := func(context.Context, *middleware.ActingInput) (*tool.ToolResponse, error) {
				executed++
				return tool.NewTextResponse("scripted human approval"), nil
			}
			call := &middleware.ActingInput{AgentName: in.AgentName, ToolCall: in.ToolCall}
			_, err := r.OnActing(ctx, call, next)
			if err != nil || executed != 1 {
				t.Fatal("advice altered already-approved execution")
			}
			if kind == "replay" {
				_, _ = r.OnActing(ctx, call, next)
			}
			rs := records(t, output)
			if kind == "cross reply" {
				if len(rs) != 0 {
					t.Fatal("advice crossed reply scope")
				}
				return
			}
			if len(rs) != 1 {
				t.Fatal("advice replayed or disappeared")
			}
			expected := "invalidated"
			if kind == "unchanged" || kind == "replay" {
				expected = "binding_checked"
			}
			if rs[0].Status != expected || c.calls.Load() != 1 {
				t.Fatalf("status=%s calls=%d", rs[0].Status, c.calls.Load())
			}
		})
	}
}

type failingAudit struct{}

func (failingAudit) Write([]byte) (int, error) { return 0, errors.New("sensitive-log-path") }
func TestAuditFailureDoesNotCreateAdvice(t *testing.T) {
	c := &testClassifier{}
	r, in, ctx, _ := fixtureReviewer(t, c)
	r.output = failingAudit{}
	d := originalAsk(r, in)
	got, err := r.OnCheckPermission(ctx, in, nextResult(d))
	if err != nil || got != d {
		t.Fatal("audit changed decision")
	}
	if ctx.Value(scopeKey{}).(*replyScope).pending != nil || r.auditError() == nil {
		t.Fatal("unrecorded advice was retained")
	}
	if strings.Contains(r.auditError().Error(), "sensitive") {
		t.Fatal("raw writer error leaked")
	}
}

func TestOfflineExampleRequiresConfirmation(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), false, true, func(string) string { t.Fatal("offline environment read"); return "" }, &output); err != nil {
		t.Fatal(err)
	}
	summaries := 0
	reviews := 0
	finish := false
	dec := json.NewDecoder(&output)
	for {
		var value map[string]any
		err := dec.Decode(&value)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if value["stage"] == "summary" {
			summaries++
			if value["confirmation_events"] != float64(1) || value["scripted_user"] != true {
				t.Fatal("missing scripted human confirmation")
			}
			want := float64(0)
			if value["confirmed"] == true {
				want = 1
			}
			if value["synthetic_tool_executions"] != want {
				t.Fatal("recommendation bypassed user")
			}
		}
		if value["stage"] == "review" {
			reviews++
			if value["recommendation"] != "allow_once" || value["final"] != "ask" {
				t.Fatal("advice became approval")
			}
		}
		if value["stage"] == "finish" {
			finish = true
		}
	}
	if summaries != 2 || reviews != 2 || !finish {
		t.Fatal("missing review/denial cleanup evidence")
	}
}
func TestDisabledAndInvalidLiveConfiguration(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), false, false, func(string) string { t.Fatal("disabled environment read"); return "" }, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), `"stage":"review"`) {
		t.Fatal("disabled feature reviewed")
	}
	for _, enabled := range []bool{false, true} {
		if err := run(context.Background(), true, enabled, func(string) string { return "" }, io.Discard); err == nil {
			t.Fatal("live ran without explicit settings")
		}
	}
}

func TestConcurrentReviewCapacity(t *testing.T) {
	c := &testClassifier{}
	r, in, ctx, output := fixtureReviewer(t, c)
	started := make(chan struct{})
	release := make(chan struct{})
	c.act = func(c context.Context, req classifier.Request, _ classifier.DispatchLimits, begin func(classifier.AttemptKind) error) (*classifier.Response, error) {
		close(started)
		<-release
		return validReply(c, req, begin)
	}
	d := originalAsk(r, in)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = r.OnCheckPermission(ctx, in, nextResult(d)) }()
	<-started
	other := *in
	other.ToolCall.ID = "other"
	scope := &replyScope{id: 2, intentOK: true, seen: make(map[string]bool)}
	_, err := r.OnCheckPermission(context.WithValue(context.Background(), scopeKey{}, scope), &other, nextResult(d))
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if c.calls.Load() != 1 || !strings.Contains(output.String(), `"status":"busy"`) {
		t.Fatal("review concurrency was not bounded")
	}
}

// A distinct backend identity must be rejected even with the same methods.
type foreignBackend struct{ tool.LocalBackend }
