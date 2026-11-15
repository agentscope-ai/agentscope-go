package classifier_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

func request() c.Request {
	return c.Request{OperationID: "op-1", State: json.RawMessage(`"Summarize this text"`), Questions: map[string]c.Question{
		"binary": &c.BinaryQuestion{Instructions: "Is this a summarization task?"},
		"choice": &c.ChoiceQuestion{Instructions: "Choose a category.", Choices: map[string]string{"summary": "Summarization", "code": "Programming"}},
		"score":  &c.ScoreQuestion{Instructions: "Rate complexity.", Levels: []string{"Simple", "Moderate", "Complex"}},
	}}
}
func response() c.Response {
	return c.Response{OperationID: "op-1", Model: "fixture-v1", Answers: map[string]c.Answer{
		"binary": &c.BinaryAnswer{Probability: 0.9},
		"choice": &c.ChoiceAnswer{Choice: "summary", Confidence: 0.7, Probabilities: map[string]float64{"summary": 0.8, "code": 0.2}},
		"score":  &c.ScoreAnswer{Score: 0.7, Confidence: 0.6, Probabilities: []float64{0.4, 0.5, 0.1}},
	}}
}
func TestRequestValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*c.Request)
	}{
		{"operation", func(r *c.Request) { r.OperationID = " " }},
		{"empty state", func(r *c.Request) { r.State = nil }},
		{"malformed", func(r *c.Request) { r.State = json.RawMessage(`{`) }},
		{"array", func(r *c.Request) { r.State = json.RawMessage(`[]`) }},
		{"null", func(r *c.Request) { r.State = json.RawMessage(`null`) }},
		{"number", func(r *c.Request) { r.State = json.RawMessage(`3`) }},
		{"blank text", func(r *c.Request) { r.State = json.RawMessage(`"  "`) }},
		{"no questions", func(r *c.Request) { r.Questions = nil }},
		{"blank ID", func(r *c.Request) { r.Questions[" "] = &c.BinaryQuestion{Instructions: "Question"} }},
		{"nil question", func(r *c.Request) { r.Questions["binary"] = nil }},
		{"typed nil question", func(r *c.Request) { r.Questions["binary"] = (*c.BinaryQuestion)(nil) }},
		{"no instructions", func(r *c.Request) { r.Questions["binary"] = &c.BinaryQuestion{} }},
		{"no choice instructions", func(r *c.Request) { r.Questions["choice"].(*c.ChoiceQuestion).Instructions = "" }},
		{"no choices", func(r *c.Request) { r.Questions["choice"].(*c.ChoiceQuestion).Choices = nil }},
		{"blank choice", func(r *c.Request) { r.Questions["choice"].(*c.ChoiceQuestion).Choices[""] = "" }},
		{"no score instructions", func(r *c.Request) { r.Questions["score"].(*c.ScoreQuestion).Instructions = " " }},
		{"no levels", func(r *c.Request) { r.Questions["score"].(*c.ScoreQuestion).Levels = nil }},
		{"blank level", func(r *c.Request) { r.Questions["score"].(*c.ScoreQuestion).Levels[0] = " " }},
		{"duplicate level", func(r *c.Request) { r.Questions["score"].(*c.ScoreQuestion).Levels[1] = "Simple" }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := request()
			tt.mutate(&r)
			err := c.ValidateRequest(r)
			var typed *ae.AgentError
			if !errors.Is(err, c.ErrInvalidRequest) || !errors.As(err, &typed) {
				t.Fatalf("want structured request error, got %v", err)
			}
		})
	}
	for _, state := range []string{`"task"`, `{"task":"hello","history":["world"]}`, `{}`} {
		r := request()
		r.State = json.RawMessage(state)
		if err := c.ValidateRequest(r); err != nil {
			t.Fatal(err)
		}
	}
	r := request()
	r.Questions = map[string]c.Question{"one": &c.ChoiceQuestion{Instructions: "Choose.", Choices: map[string]string{"a": ""}}, "score": &c.ScoreQuestion{Instructions: "Rate.", Levels: []string{"Only level"}}}
	if err := c.ValidateRequest(r); err != nil {
		t.Fatal(err)
	}
}
func TestResponseValidation(t *testing.T) {
	if err := c.ValidateResponse(request(), ptr(response())); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*c.Response)
	}{
		{"wrong operation", func(r *c.Response) { r.OperationID = "other" }},
		{"no model", func(r *c.Response) { r.Model = " " }},
		{"missing", func(r *c.Response) { delete(r.Answers, "binary") }},
		{"extra", func(r *c.Response) { r.Answers["extra"] = &c.BinaryAnswer{Probability: 0.5} }},
		{"nil", func(r *c.Response) { r.Answers["binary"] = nil }},
		{"typed nil", func(r *c.Response) { r.Answers["binary"] = (*c.BinaryAnswer)(nil) }},
		{"wrong type", func(r *c.Response) { r.Answers["binary"] = &c.ScoreAnswer{Score: 0} }},
		{"negative probability", func(r *c.Response) { r.Answers["binary"].(*c.BinaryAnswer).Probability = -0.1 }},
		{"over one", func(r *c.Response) { r.Answers["binary"].(*c.BinaryAnswer).Probability = 1.1 }},
		{"nan", func(r *c.Response) { r.Answers["binary"].(*c.BinaryAnswer).Probability = math.NaN() }},
		{"infinity", func(r *c.Response) { r.Answers["binary"].(*c.BinaryAnswer).Probability = math.Inf(1) }},
		{"unknown choice", func(r *c.Response) { r.Answers["choice"].(*c.ChoiceAnswer).Choice = "other" }},
		{"not maximum", func(r *c.Response) { r.Answers["choice"].(*c.ChoiceAnswer).Choice = "code" }},
		{"choice keys", func(r *c.Response) {
			a := r.Answers["choice"].(*c.ChoiceAnswer)
			delete(a.Probabilities, "code")
			a.Probabilities["other"] = 0.2
		}},
		{"missing choice", func(r *c.Response) { delete(r.Answers["choice"].(*c.ChoiceAnswer).Probabilities, "code") }},
		{"bad sum", func(r *c.Response) { r.Answers["choice"].(*c.ChoiceAnswer).Probabilities["code"] = 0.1 }},
		{"choice confidence", func(r *c.Response) { r.Answers["choice"].(*c.ChoiceAnswer).Confidence = math.NaN() }},
		{"score confidence", func(r *c.Response) { r.Answers["score"].(*c.ScoreAnswer).Confidence = 1.1 }},
		{"missing score level", func(r *c.Response) { r.Answers["score"].(*c.ScoreAnswer).Probabilities = []float64{0.5, 0.5} }},
		{"score probability", func(r *c.Response) { r.Answers["score"].(*c.ScoreAnswer).Probabilities = []float64{0.8, 0.3, -0.1} }},
		{"score sum", func(r *c.Response) { r.Answers["score"].(*c.ScoreAnswer).Probabilities = []float64{0.1, 0.1, 0.1} }},
		{"score not expectation", func(r *c.Response) { r.Answers["score"].(*c.ScoreAnswer).Score = 0.5 }},
		{"score range", func(r *c.Response) { r.Answers["score"].(*c.ScoreAnswer).Score = 3 }},
		{"score nan", func(r *c.Response) { r.Answers["score"].(*c.ScoreAnswer).Score = math.NaN() }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := response()
			tt.mutate(&r)
			if err := c.ValidateResponse(request(), &r); !errors.Is(err, c.ErrInvalidResponse) {
				t.Fatalf("want response error, got %v", err)
			}
		})
	}
	if err := c.ValidateResponse(request(), nil); !errors.Is(err, c.ErrInvalidResponse) {
		t.Fatal(err)
	}
	r := response()
	a := r.Answers["choice"].(*c.ChoiceAnswer)
	a.Probabilities = map[string]float64{"code": 0.5, "summary": 0.5}
	if err := c.ValidateResponse(request(), &r); err != nil {
		t.Fatalf("tie: %v", err)
	}
}
func ptr[T any](x T) *T { return &x }
func TestReportValidation(t *testing.T) {
	r := c.Response{OperationID: "op-1", Attempts: []c.Attempt{{Kind: c.PhysicalAttempt, Duration: time.Second, Usage: &c.Usage{InputTokens: ptr(int64(0))}, Err: context.Canceled}}}
	if err := c.ValidateReport(request(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Attempts[0].Usage.OutputTokens != nil || *r.Attempts[0].Usage.InputTokens != 0 {
		t.Fatal("unknown usage became zero")
	}
	cases := []struct {
		name   string
		mutate func(*c.Response)
	}{
		{"multiple attempts", func(r *c.Response) { r.Attempts = append(r.Attempts, r.Attempts[0]) }},
		{"unknown kind", func(r *c.Response) { r.Attempts[0].Kind = "" }},
		{"duration", func(r *c.Response) { r.Attempts[0].Duration = -1 }},
		{"input tokens", func(r *c.Response) { r.Attempts[0].Usage.InputTokens = ptr(int64(-1)) }},
		{"output tokens", func(r *c.Response) { r.Attempts[0].Usage.OutputTokens = ptr(int64(-1)) }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			x := c.Response{OperationID: "op-1", Attempts: []c.Attempt{{Kind: c.PhysicalAttempt, Usage: &c.Usage{}}}}
			tt.mutate(&x)
			if err := c.ValidateReport(request(), &x); !errors.Is(err, c.ErrInvalidResponse) {
				t.Fatal(err)
			}
		})
	}
}
func TestFakeSuccessAndOwnership(t *testing.T) {
	preset := response()
	preset.Attempts = []c.Attempt{{Kind: c.PhysicalAttempt, Usage: &c.Usage{InputTokens: ptr(int64(7))}}}
	f := c.NewFake(&c.FakeConfig{Response: preset})
	var _ c.Classifier = f
	preset.Answers["binary"].(*c.BinaryAnswer).Probability = 0
	*preset.Attempts[0].Usage.InputTokens = 99
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := f.Classify(context.Background(), request())
			if err != nil {
				t.Error(err)
				return
			}
			if len(r.Attempts) != 1 || r.Attempts[0].Kind != c.SimulatedAttempt || *r.Attempts[0].Usage.InputTokens != 7 {
				t.Errorf("bad report: %+v", r)
			}
			if r.Answers["binary"].(*c.BinaryAnswer).Probability != 0.9 {
				t.Error("shared preset")
			}
			r.Answers["binary"].(*c.BinaryAnswer).Probability = 0
			r.Answers["choice"].(*c.ChoiceAnswer).Probabilities["code"] = 1
			r.Answers["score"].(*c.ScoreAnswer).Probabilities[0] = 1
			*r.Attempts[0].Usage.InputTokens = 50
		}()
	}
	wg.Wait()
}
func assertCancellationError(t *testing.T, err, cause error) {
	t.Helper()
	if !errors.Is(err, cause) {
		t.Fatalf("lost cancellation cause: %v", err)
	}
	var typed *ae.AgentError
	if !errors.As(err, &typed) || typed.Category != ae.CategoryModel || typed.Code != "classifier.canceled" {
		t.Fatalf("want model/classifier.canceled, got %#v", typed)
	}
	if ae.IsRetryable(err) {
		t.Fatalf("cancellation must not be retryable: %v", err)
	}
}

func TestFakeFailures(t *testing.T) {
	cause := fmt.Errorf("transport: %w", context.DeadlineExceeded)
	f := c.NewFake(&c.FakeConfig{Response: response(), Error: cause})
	r, err := f.Classify(context.Background(), request())
	assertCancellationError(t, err, context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) || len(r.Attempts) != 1 || !errors.Is(r.Attempts[0].Err, cause) {
		t.Fatalf("lost dispatched error: %+v %v", r, err)
	}
	invalid := response()
	delete(invalid.Answers, "binary")
	r, err = c.NewFake(&c.FakeConfig{Response: invalid}).Classify(context.Background(), request())
	if !errors.Is(err, c.ErrInvalidResponse) || len(r.Attempts) != 1 {
		t.Fatalf("invalid response: %+v %v", r, err)
	}
	bad := request()
	bad.Questions = nil
	r, err = f.Classify(context.Background(), bad)
	if !errors.Is(err, c.ErrInvalidRequest) || len(r.Attempts) != 0 {
		t.Fatalf("counted predispatch: %+v %v", r, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err = f.Classify(ctx, request())
	assertCancellationError(t, err, context.Canceled)
	if !errors.Is(err, context.Canceled) || len(r.Attempts) != 0 {
		t.Fatalf("counted canceled predispatch: %+v %v", r, err)
	}
	for _, missingContext := range []context.Context{nil} {
		r, err = f.Classify(missingContext, request())
		if !errors.Is(err, c.ErrInvalidRequest) || len(r.Attempts) != 0 {
			t.Fatalf("nil context: %+v %v", r, err)
		}
	}
}

// deadlineOnCancel lets the test expire a deadline only after simulated dispatch.
type deadlineOnCancel struct{ context.Context }

func (ctx deadlineOnCancel) Err() error {
	if ctx.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

func TestFakeCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			var ctx context.Context = parent
			defer cancel()
			want := context.Canceled
			if deadline {
				ctx = deadlineOnCancel{parent}
				want = context.DeadlineExceeded
			}
			configuredError := errors.New("preset error overridden by cancellation")
			f := c.NewFake(&c.FakeConfig{Response: response(), Error: configuredError, WaitForCancel: true})
			started := f.Started()
			done := make(chan struct{})
			var r *c.Response
			var err error
			go func() {
				defer close(done)
				r, err = f.Classify(ctx, request())
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("fake did not start")
			}
			cancel()
			select {
			case <-done:
				if errors.Is(err, configuredError) {
					t.Error("WaitForCancel did not override the configured error")
				}
				assertCancellationError(t, err, want)
				if len(r.Attempts) == 1 {
					assertCancellationError(t, r.Attempts[0].Err, want)
				}
				if !errors.Is(err, want) || len(r.Attempts) != 1 || r.Attempts[0].Kind != c.SimulatedAttempt || !errors.Is(r.Attempts[0].Err, want) {
					t.Errorf("lost cancellation accounting: %+v %v", r, err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation hung")
			}
		})
	}
}

func TestSuccessAccountingValidation(t *testing.T) {
	r := response()
	r.Attempts = []c.Attempt{{Kind: c.PhysicalAttempt, RequestedModel: "alias", Model: "fixture-v1"}}
	if err := c.ValidateResponse(request(), &r); err != nil {
		t.Fatal(err)
	}
	r.Attempts[0].Model = "other-version"
	if err := c.ValidateResponse(request(), &r); !errors.Is(err, c.ErrInvalidResponse) {
		t.Fatal("accepted contradictory actual models")
	}
	r.Attempts[0].Model = ""
	r.Attempts[0].Err = context.Canceled
	if err := c.ValidateResponse(request(), &r); !errors.Is(err, c.ErrInvalidResponse) {
		t.Fatal("accepted failure as successful answers")
	}
	if err := c.ValidateReport(request(), &r); err != nil {
		t.Fatalf("failed attempt report: %v", err)
	}
	req := request()
	req.Questions = nil
	if err := c.ValidateResponse(req, &r); !errors.Is(err, c.ErrInvalidRequest) {
		t.Fatal("accepted invalid request")
	}
	r.Attempts[0].Kind = "invalid"
	if err := c.ValidateResponse(request(), &r); !errors.Is(err, c.ErrInvalidResponse) {
		t.Fatal("accepted invalid accounting")
	}
	r = response()
	delete(r.Answers, "binary")
	r.Answers["other"] = &c.BinaryAnswer{Probability: 0.5}
	if err := c.ValidateResponse(request(), &r); !errors.Is(err, c.ErrInvalidResponse) {
		t.Fatal("accepted substituted answer ID")
	}
}

func TestFakeRetainsUsageAndUnknownIdentityOnFailure(t *testing.T) {
	fixture := c.Response{Attempts: []c.Attempt{{RequestedModel: "provider-alias", Usage: &c.Usage{InputTokens: ptr(int64(3)), OutputTokens: ptr(int64(0))}}}}
	cause := errors.New("connection closed before response")
	f := c.NewFake(&c.FakeConfig{Response: fixture, Error: cause})
	out, err := f.Classify(context.Background(), request())
	if !errors.Is(err, cause) || out.Model != "" || out.Attempts[0].Model != "" || out.Attempts[0].RequestedModel != "provider-alias" {
		t.Fatalf("fabricated model/lost cause: %+v %v", out, err)
	}
	if err := c.ValidateReport(request(), out); err != nil {
		t.Fatal(err)
	}
	if *out.Attempts[0].Usage.InputTokens != 3 || *out.Attempts[0].Usage.OutputTokens != 0 {
		t.Fatal("lost partial known usage")
	}
	// Copies include both optional counts, even on failed calls.
	*out.Attempts[0].Usage.OutputTokens = 99
	again, _ := f.Classify(context.Background(), request())
	if *again.Attempts[0].Usage.OutputTokens != 0 {
		t.Fatal("shared output count")
	}
	fixture = response()
	delete(fixture.Answers, "score")
	fixture.Attempts = []c.Attempt{{Usage: &c.Usage{InputTokens: ptr(int64(8))}}}
	out, err = c.NewFake(&c.FakeConfig{Response: fixture}).Classify(context.Background(), request())
	if !errors.Is(err, c.ErrInvalidResponse) || *out.Attempts[0].Usage.InputTokens != 8 || !errors.Is(out.Attempts[0].Err, c.ErrInvalidResponse) {
		t.Fatalf("invalid answers lost usage/outcome: %+v %v", out, err)
	}
	fixture = c.Response{Attempts: []c.Attempt{{Usage: &c.Usage{InputTokens: ptr(int64(-1))}}}}
	out, err = c.NewFake(&c.FakeConfig{Response: fixture, Error: cause}).Classify(context.Background(), request())
	if !errors.Is(err, cause) || !errors.Is(err, c.ErrInvalidResponse) || len(out.Attempts) != 1 {
		t.Fatalf("lost simultaneous fixture/transport errors: %+v %v", out, err)
	}
}

func TestTypedNilVariants(t *testing.T) {
	for _, q := range []c.Question{(*c.ChoiceQuestion)(nil), (*c.ScoreQuestion)(nil)} {
		r := request()
		r.Questions["binary"] = q
		if err := c.ValidateRequest(r); !errors.Is(err, c.ErrInvalidRequest) {
			t.Fatal("accepted nil question")
		}
	}
	for _, a := range []c.Answer{(*c.BinaryAnswer)(nil), (*c.ChoiceAnswer)(nil), (*c.ScoreAnswer)(nil), nil} {
		fixture := response()
		fixture.Answers["binary"] = a
		out, err := c.NewFake(&c.FakeConfig{Response: fixture}).Classify(context.Background(), request())
		if !errors.Is(err, c.ErrInvalidResponse) || len(out.Attempts) != 1 {
			t.Fatalf("nil fixture: %+v %v", out, err)
		}
	}
}

func TestProbabilityBoundaries(t *testing.T) {
	for _, p := range []float64{0, 1} {
		r := response()
		r.Answers["binary"].(*c.BinaryAnswer).Probability = p
		if err := c.ValidateResponse(request(), &r); err != nil {
			t.Fatal(err)
		}
	}
	r := response()
	r.Answers["choice"].(*c.ChoiceAnswer).Probabilities["code"] += c.ProbabilityTolerance / 2
	if err := c.ValidateResponse(request(), &r); err != nil {
		t.Fatal("rejected harmless probability rounding", err)
	}
	r.Answers["choice"].(*c.ChoiceAnswer).Probabilities["code"] += 2 * c.ProbabilityTolerance
	if err := c.ValidateResponse(request(), &r); !errors.Is(err, c.ErrInvalidResponse) {
		t.Fatal("accepted sum outside tolerance")
	}
	req := c.Request{OperationID: "op-1", State: json.RawMessage(`"task"`), Questions: map[string]c.Question{"one": &c.ScoreQuestion{Instructions: "Rate.", Levels: []string{"Only"}}}}
	out := c.Response{OperationID: "op-1", Model: "fixture-v1", Answers: map[string]c.Answer{"one": &c.ScoreAnswer{Score: 0, Confidence: 1, Probabilities: []float64{1}}}}
	if err := c.ValidateResponse(req, &out); err != nil {
		t.Fatal(err)
	}
}

func TestFakeUnconfigured(t *testing.T) {
	for _, f := range []*c.Fake{nil, {}} {
		out, err := f.Classify(context.Background(), request())
		if !errors.Is(err, c.ErrInvalidRequest) || len(out.Attempts) != 0 {
			t.Fatalf("unconfigured fake: %+v %v", out, err)
		}
	}
	out, err := c.NewFake(nil).Classify(context.Background(), request())
	if !errors.Is(err, c.ErrInvalidResponse) || len(out.Attempts) != 1 {
		t.Fatalf("empty fixture: %+v %v", out, err)
	}
}

func TestExpiredDeadlineBeforeDispatch(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	f := c.NewFake(&c.FakeConfig{Response: response()})
	out, err := f.Classify(ctx, request())
	assertCancellationError(t, err, context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) || len(out.Attempts) != 0 {
		t.Fatalf("expired deadline dispatched: %+v %v", out, err)
	}
	select {
	case <-f.Started():
		t.Fatal("reported dispatch for expired deadline")
	default:
	}
}
