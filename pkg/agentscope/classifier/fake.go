package classifier

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

// FakeConfig specifies a reusable offline fixture. Response.Answers and the
// first attempt's usage/requested model are copied; attempts are always rebuilt
// as simulations. Error takes precedence over answer validation. WaitForCancel
// waits for ctx cancellation after dispatch, then returns that context error,
// overriding any configured Error.
// Error objects are shared and must be immutable. No external service is used.
type FakeConfig struct {
	Response      Response
	Error         error
	WaitForCancel bool
}

// Fake implements Classifier with immutable presets and independent results.
// It is safe for concurrent calls and must be constructed with NewFake.
type Fake struct {
	config  FakeConfig
	started chan struct{}
	once    sync.Once
}

// NewFake snapshots config. Invalid preset answers are intentionally accepted
// here so Classify can exercise response-validation failures with accounting.
// A nil config supplies an empty fixture.
func NewFake(config *FakeConfig) *Fake {
	var preset FakeConfig
	if config != nil {
		preset = *config
		preset.Response = cloneResponse(config.Response)
	}
	return &Fake{config: preset, started: make(chan struct{})}
}

// Started closes once, at the first simulated dispatch. It is useful for
// synchronizing cancellation tests; callers must not close the channel.
// The receiver must be created with NewFake. A nil receiver panics; a zero-value
// Fake returns a nil channel that never closes.
func (f *Fake) Started() <-chan struct{} { return f.started }

// Classify validates input, simulates one dispatch, and returns a fresh report.
// Pre-dispatch failures have no attempts. Failure/cancellation after dispatch
// retains one simulated attempt. It never retries or logs task data.
func (f *Fake) Classify(ctx context.Context, r Request) (*Response, error) {
	out := &Response{OperationID: r.OperationID}
	if f == nil || f.started == nil {
		return out, invalidRequest("fake must be constructed with NewFake")
	}
	if ctx == nil {
		return out, invalidRequest("context is required")
	}
	if err := ctx.Err(); err != nil {
		return out, ae.Wrap(err, ae.CategoryModel, "classifier.canceled", "classification canceled before dispatch")
	}
	if err := ValidateRequest(r); err != nil {
		return out, err
	}
	start := time.Now()
	preset := cloneResponse(f.config.Response)
	preset.OperationID = r.OperationID
	out = &preset
	attempt := Attempt{Kind: SimulatedAttempt, Model: out.Model}
	if len(out.Attempts) > 0 {
		attempt.RequestedModel = out.Attempts[0].RequestedModel
		attempt.Usage = out.Attempts[0].Usage
	}
	if err := ctx.Err(); err != nil {
		return &Response{OperationID: r.OperationID}, ae.Wrap(err, ae.CategoryModel, "classifier.canceled", "classification canceled before dispatch")
	}
	out.Attempts = []Attempt{attempt}
	f.once.Do(func() { close(f.started) })
	err := f.config.Error
	if f.config.WaitForCancel {
		<-ctx.Done()
		err = ctx.Err()
	} else if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			err = ae.Wrap(err, ae.CategoryModel, "classifier.canceled", "classification canceled after dispatch")
		} else {
			err = ae.Wrap(err, ae.CategoryModel, "classifier.failed", "classification attempt failed")
		}
	}
	out.Attempts[0].Duration = time.Since(start)
	if err == nil {
		err = ValidateResponse(r, out)
	} else if reportErr := ValidateReport(r, out); reportErr != nil {
		// Keep both errors matchable if a fixture itself has invalid accounting.
		err = errors.Join(err, reportErr)
	}
	out.Attempts[0].Err = err
	return out, err
}

func cloneResponse(in Response) Response {
	out := in
	if in.Answers != nil {
		out.Answers = make(map[string]Answer, len(in.Answers))
		for id, a := range in.Answers {
			switch a := a.(type) {
			case *BinaryAnswer:
				if a != nil {
					v := *a
					out.Answers[id] = &v
				} else {
					out.Answers[id] = a
				}
			case *ChoiceAnswer:
				if a != nil {
					v := *a
					v.Probabilities = maps.Clone(a.Probabilities)
					out.Answers[id] = &v
				} else {
					out.Answers[id] = a
				}
			case *ScoreAnswer:
				if a != nil {
					v := *a
					v.Probabilities = slices.Clone(a.Probabilities)
					out.Answers[id] = &v
				} else {
					out.Answers[id] = a
				}
			default:
				out.Answers[id] = nil
			}
		}
	}
	out.Attempts = slices.Clone(in.Attempts)
	for i := range out.Attempts {
		if usage := in.Attempts[i].Usage; usage != nil {
			v := *usage
			if usage.InputTokens != nil {
				x := *usage.InputTokens
				v.InputTokens = &x
			}
			if usage.OutputTokens != nil {
				x := *usage.OutputTokens
				v.OutputTokens = &x
			}
			out.Attempts[i].Usage = &v
		}
	}
	return out
}
