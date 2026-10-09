package classifier

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"unicode/utf8"

	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

// ProbabilityTolerance is the absolute tolerance for distribution sums. Score
// expectations use this tolerance multiplied by max(1, highest rubric level).
// Validation never normalizes or repairs provider values.
const ProbabilityTolerance = 1e-6

func invalidRequest(message string) error {
	return ae.Newf(ae.CategoryConfig, ErrInvalidRequest.Code, "invalid classifier request: %s", message)
}
func invalidResponse(message string) error {
	return ae.Newf(ae.CategoryModel, ErrInvalidResponse.Code, "invalid classifier response: %s", message)
}
func nonblank(s string) bool     { return utf8.ValidString(s) && strings.TrimSpace(s) != "" }
func probability(v float64) bool { return !math.IsNaN(v) && v >= 0 && v <= 1 }

// ValidateRequest checks state and typed questions before an adapter constructs
// any provider request. Validation errors omit state and question contents.
func ValidateRequest(r Request) error {
	if !nonblank(r.OperationID) {
		return invalidRequest("operation ID is required")
	}
	state := bytes.TrimSpace(r.State)
	if !utf8.Valid(state) || !json.Valid(state) {
		return invalidRequest("state must be valid JSON")
	}
	switch state[0] {
	case '"':
		var s string
		if err := json.Unmarshal(state, &s); err != nil || !nonblank(s) {
			return invalidRequest("state text must not be blank")
		}
	case '{':
	default:
		return invalidRequest("state must be a JSON string or object")
	}
	if len(r.Questions) == 0 {
		return invalidRequest("at least one question is required")
	}
	for id, q := range r.Questions {
		if !nonblank(id) {
			return invalidRequest("question ID must not be blank")
		}
		switch q := q.(type) {
		case *BinaryQuestion:
			if q == nil || !nonblank(q.Instructions) {
				return invalidRequest("binary question requires instructions")
			}
		case *ChoiceQuestion:
			if q == nil || !nonblank(q.Instructions) || len(q.Choices) == 0 {
				return invalidRequest("choice question requires instructions and candidates")
			}
			for key := range q.Choices {
				if !nonblank(key) {
					return invalidRequest("candidate ID must not be blank")
				}
			}
		case *ScoreQuestion:
			if q == nil || !nonblank(q.Instructions) || len(q.Levels) == 0 {
				return invalidRequest("score question requires instructions and levels")
			}
			seen := make(map[string]bool, len(q.Levels))
			for _, level := range q.Levels {
				if !nonblank(level) || seen[level] {
					return invalidRequest("score levels must be nonblank and distinct")
				}
				seen[level] = true
			}
		default:
			return invalidRequest("question must have a supported type")
		}
	}
	return nil
}

// ValidateReport checks accounting independently of answer validity. Use it on
// failed calls too: missing answers and unknown actual model identity are legal
// then. A report must echo OperationID and contain at most one attempt. This
// cannot prove that an adapter really dispatched a request; adapters must record
// attempts at their dispatch boundary and test that boundary independently.
func ValidateReport(r Request, out *Response) error {
	if out == nil {
		return invalidResponse("missing report")
	}
	if !nonblank(r.OperationID) || out.OperationID != r.OperationID {
		return invalidResponse("operation ID does not match")
	}
	if len(out.Attempts) > 1 {
		return invalidResponse("primitive calls must not retry")
	}
	for _, a := range out.Attempts {
		if a.Kind != PhysicalAttempt && a.Kind != SimulatedAttempt {
			return invalidResponse("unknown attempt kind")
		}
		if a.Duration < 0 {
			return invalidResponse("negative attempt duration")
		}
		if a.Usage != nil {
			if a.Usage.InputTokens != nil && *a.Usage.InputTokens < 0 {
				return invalidResponse("negative input usage")
			}
			if a.Usage.OutputTokens != nil && *a.Usage.OutputTokens < 0 {
				return invalidResponse("negative output usage")
			}
		}
	}
	return nil
}

// ValidateResponse validates a successful result against its request, including
// all answers, concrete model identity and accounting. Errors never select a
// default answer or model. Invalid results can still carry valid attempt usage.
func ValidateResponse(r Request, out *Response) error {
	if err := ValidateRequest(r); err != nil {
		return err
	}
	if err := ValidateReport(r, out); err != nil {
		return err
	}
	if !nonblank(out.Model) {
		return invalidResponse("actual model identity is required on success")
	}
	for _, a := range out.Attempts {
		if a.Err != nil {
			return invalidResponse("successful result contains a failed attempt")
		}
		if a.Model != "" && a.Model != out.Model {
			return invalidResponse("actual model identities disagree")
		}
	}
	if len(out.Answers) != len(r.Questions) {
		return invalidResponse("answer IDs must exactly match question IDs")
	}
	for id, q := range r.Questions {
		answer, ok := out.Answers[id]
		if !ok {
			return invalidResponse("missing answer")
		}
		if err := validateAnswer(q, answer); err != nil {
			return err
		}
	}
	return nil
}
func validateAnswer(q Question, answer Answer) error {
	switch q := q.(type) {
	case *BinaryQuestion:
		a, ok := answer.(*BinaryAnswer)
		if !ok || a == nil || !probability(a.Probability) {
			return invalidResponse("invalid binary answer")
		}
	case *ChoiceQuestion:
		a, ok := answer.(*ChoiceAnswer)
		if !ok || a == nil || !probability(a.Confidence) {
			return invalidResponse("invalid choice answer or confidence")
		}
		if _, ok := q.Choices[a.Choice]; !ok {
			return invalidResponse("selected candidate was not requested")
		}
		if len(a.Probabilities) != len(q.Choices) {
			return invalidResponse("choice distribution must cover all candidates")
		}
		total := 0.0
		for key := range q.Choices {
			p, ok := a.Probabilities[key]
			if !ok || !probability(p) {
				return invalidResponse("invalid candidate probability")
			}
			if p > a.Probabilities[a.Choice] {
				return invalidResponse("selected candidate is not a maximum")
			}
			total += p
		}
		if math.Abs(total-1) > ProbabilityTolerance {
			return invalidResponse("candidate probabilities must sum to one")
		}
	case *ScoreQuestion:
		a, ok := answer.(*ScoreAnswer)
		if !ok || a == nil || !probability(a.Confidence) {
			return invalidResponse("invalid score answer or confidence")
		}
		maxLevel := float64(len(q.Levels) - 1)
		if math.IsNaN(a.Score) || a.Score < 0 || a.Score > maxLevel {
			return invalidResponse("score is outside rubric range")
		}
		if len(a.Probabilities) != len(q.Levels) {
			return invalidResponse("score distribution must cover all levels")
		}
		total, expected := 0.0, 0.0
		for i, p := range a.Probabilities {
			if !probability(p) {
				return invalidResponse("invalid level probability")
			}
			total += p
			expected += float64(i) * p
		}
		if math.Abs(total-1) > ProbabilityTolerance {
			return invalidResponse("level probabilities must sum to one")
		}
		if math.Abs(a.Score-expected) > ProbabilityTolerance*math.Max(1, maxLevel) {
			return invalidResponse("score differs from expected rubric level")
		}
	}
	return nil
}
