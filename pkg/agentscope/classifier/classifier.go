// Package classifier defines experimental, provider-independent probabilistic
// classification. It does not select or invoke a generation model.
package classifier

import (
	"context"
	"encoding/json"
	"time"

	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

// Classifier evaluates questions against shared state independently of ChatModel.
// One call is one attempt at a logical operation identified by OperationID. It
// must validate requests before dispatch, honor ctx, and never retry internally.
// Implementations return a non-nil Response even on errors, retaining any
// dispatched attempt and known usage. A pre-dispatch failure has zero attempts.
// Caller cancellation and errors whose chains match context.Canceled or
// context.DeadlineExceeded via errors.Is use CategoryModel with code
// classifier.canceled and are non-retryable; messages distinguish pre- and
// post-dispatch cancellation. Preserve the context cause for errors.Is.
// Other attempt failures use classifier.failed; validation errors retain their
// validation codes. Adapters must derive overall request deadlines from ctx,
// rather than impose shorter independent per-attempt deadlines that would be
// reported as non-retryable cancellation.
//
// Callers must check the error before using answers. Implementations must not
// mutate requests; callers must not mutate them while a call is in progress.
// These Go types do not define a provider or persistence JSON wire format.
type Classifier interface {
	Classify(ctx context.Context, request Request) (*Response, error)
}

// Request contains one logical classification operation. Repeated primitive
// calls for the same operation reuse OperationID; retry policy belongs outside
// this package. IDs correlate answers only: questions belong in Instructions.
type Request struct {
	OperationID string
	// State is a JSON string or object, not a chat message or arbitrary Go value.
	// Hosts are responsible for authorizing and projecting data before use.
	State     json.RawMessage
	Questions map[string]Question
}

// Question is a BinaryQuestion, ChoiceQuestion, or ScoreQuestion pointer.
type Question interface{ isQuestion() }

// BinaryQuestion (Noul) asks for the probability of a proposition being true.
type BinaryQuestion struct {
	Instructions string
	// True and False optionally describe the positive and negative outcomes.
	True  string
	False string
}

func (*BinaryQuestion) isQuestion() {}

// ChoiceQuestion asks for one named alternative and its marginal distribution.
// Choice IDs are nonblank and descriptions are optional. At least one is needed.
type ChoiceQuestion struct {
	Instructions string
	Choices      map[string]string
}

func (*ChoiceQuestion) isQuestion() {}

// ScoreQuestion asks for an expected score on an ordered rubric, starting at
// zero. Levels must contain at least one nonblank, distinct description.
type ScoreQuestion struct {
	Instructions string
	Levels       []string
}

func (*ScoreQuestion) isQuestion() {}

// Answer is a BinaryAnswer, ChoiceAnswer, or ScoreAnswer pointer. Separate
// answers are marginal decisions, not a joint probability distribution.
type Answer interface{ isAnswer() }

// BinaryAnswer reports a positive-outcome probability in [0,1].
type BinaryAnswer struct{ Probability float64 }

func (*BinaryAnswer) isAnswer() {}

// ChoiceAnswer reports a highest-probability candidate (ties are allowed).
// Confidence is a separate provider-reported value in [0,1]; it need not equal
// the chosen candidate's probability. Probabilities contains every choice.
type ChoiceAnswer struct {
	Choice        string
	Confidence    float64
	Probabilities map[string]float64
}

func (*ChoiceAnswer) isAnswer() {}

// ScoreAnswer reports the probability-weighted expected zero-based rubric
// level. Probabilities has exactly one entry per requested level, in order.
// Confidence is provider-reported and distinct from the expected score.
type ScoreAnswer struct {
	Score         float64
	Confidence    float64
	Probabilities []float64
}

func (*ScoreAnswer) isAnswer() {}

// Response contains answers and accounting for one primitive call. Model is
// the concrete model reported by the provider on success; it can be unknown on
// failure. Usage is held only in Attempts to avoid double counting. Purely local
// implementations may succeed without a request attempt.
type Response struct {
	OperationID string
	Model       string
	Answers     map[string]Answer
	Attempts    []Attempt
}

// AttemptKind distinguishes network dispatches from offline simulations.
type AttemptKind string

const (
	// PhysicalAttempt denotes one actual outbound request dispatch, even if it fails.
	PhysicalAttempt AttemptKind = "physical"
	// SimulatedAttempt denotes an offline fixture; it is never a physical request.
	SimulatedAttempt AttemptKind = "simulated"
)

// Attempt records a dispatched request or simulation. A primitive Classify
// call reports at most one; a later executor may combine calls by OperationID.
// Err is the classification outcome of this attempt, including invalid answers
// or cancellation, and supports errors.Is/As. Known usage survives such errors.
// No request body or credentials should be stored in this record.
type Attempt struct {
	Kind           AttemptKind
	RequestedModel string
	// Model is the actual provider-reported identity; empty means unknown.
	Model    string
	Duration time.Duration
	Usage    *Usage
	Err      error
}

// Usage contains provider-reported token counts, never estimates. A nil Usage
// or count means unknown. A pointer to zero means a known zero count.
type Usage struct {
	InputTokens  *int64
	OutputTokens *int64
}

// Validation error sentinels support errors.Is and errors.As(*AgentError).
var (
	ErrInvalidRequest  = &ae.AgentError{Category: ae.CategoryConfig, Code: "classifier.invalid_request", Message: "invalid classifier request"}
	ErrInvalidResponse = &ae.AgentError{Category: ae.CategoryModel, Code: "classifier.invalid_response", Message: "invalid classifier response"}
)
