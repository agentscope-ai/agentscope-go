// Package systemone contains the shared classifier wire codec. Callers validate
// requests and provider-specific limits before Encode, and validate complete
// successful responses after Decode. Decode requires one initialized attempt.
package systemone

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf8"

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
)

type question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Encode maps a validated classifier request to the System One envelope.
func Encode(model string, r c.Request, invalid func(string) error) ([]byte, error) {
	qs := make(map[string]question, len(r.Questions))
	for id, q := range r.Questions {
		switch q := q.(type) {
		case *c.BinaryQuestion:
			criteria := map[string]string{}
			if q.True != "" {
				criteria["true"] = q.True
			}
			if q.False != "" {
				criteria["false"] = q.False
			}
			w := question{Type: "noul", Instructions: q.Instructions}
			if len(criteria) > 0 {
				w.Criteria = criteria
			}
			qs[id] = w
		case *c.ChoiceQuestion:
			qs[id] = question{Type: "choice", Instructions: q.Instructions, Criteria: q.Choices}
		case *c.ScoreQuestion:
			qs[id] = question{Type: "score", Instructions: q.Instructions, Criteria: q.Levels}
		}
	}
	body, e := json.Marshal(struct {
		Model     string              `json:"model"`
		State     json.RawMessage     `json:"state"`
		Questions map[string]question `json:"questions"`
	}{model, r.State, qs})
	if e != nil {
		return nil, invalid("cannot encode request")
	}
	return body, nil
}

// Decode accounting independently of answers so a malformed answer does not
// erase valid provider usage. Invalid token fields remain unknown, not zero.
func Decode(body []byte, r c.Request, out *c.Response, invalid func(string) error) error {
	var fields map[string]json.RawMessage
	if !utf8.Valid(body) || json.Unmarshal(body, &fields) != nil || fields == nil {
		return invalid("malformed JSON response")
	}
	var bad bool
	if e := json.Unmarshal(fields["model"], &out.Model); e != nil {
		bad = true
	}
	out.Attempts[0].Model = out.Model
	if raw, ok := fields["usage"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var usage map[string]json.RawMessage
		if json.Unmarshal(raw, &usage) != nil || usage == nil {
			bad = true
		} else {
			u := &c.Usage{}
			out.Attempts[0].Usage = u
			for name, dst := range map[string]**int64{"input_tokens": &u.InputTokens, "output_tokens": &u.OutputTokens} {
				if v, ok := usage[name]; ok {
					var n *int64
					if json.Unmarshal(v, &n) != nil || (n != nil && *n < 0) {
						bad = true
					} else {
						*dst = n
					}
				}
			}
		}
	}
	var answers map[string]json.RawMessage
	if json.Unmarshal(fields["answers"], &answers) != nil || len(answers) != len(r.Questions) {
		return invalid("invalid answer map")
	}
	out.Answers = make(map[string]c.Answer, len(answers))
	for id, raw := range answers {
		q, ok := r.Questions[id]
		if !ok {
			return invalid("unexpected answer ID")
		}
		a, e := decodeAnswer(raw, q, invalid)
		if e != nil {
			return e
		}
		out.Answers[id] = a
	}
	if bad {
		return invalid("invalid model or usage")
	}
	return nil
}
func decodeAnswer(raw json.RawMessage, q c.Question, invalid func(string) error) (c.Answer, error) {
	var a struct {
		Type          string              `json:"type"`
		Noul          *float64            `json:"noul"`
		Choice        string              `json:"choice"`
		Confidence    *float64            `json:"confidence"`
		Score         *float64            `json:"score"`
		Probabilities map[string]*float64 `json:"probabilities"`
		Legend        map[string]string   `json:"legend"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return nil, invalid("malformed answer")
	}
	switch q := q.(type) {
	case *c.BinaryQuestion:
		if a.Type == "noul" && a.Noul != nil {
			return &c.BinaryAnswer{Probability: *a.Noul}, nil
		}
	case *c.ChoiceQuestion:
		if a.Type == "choice" && a.Confidence != nil {
			ps := make(map[string]float64, len(a.Probabilities))
			for k, p := range a.Probabilities {
				if p == nil {
					return nil, invalid("null candidate probability")
				}
				ps[k] = *p
			}
			return &c.ChoiceAnswer{Choice: a.Choice, Confidence: *a.Confidence, Probabilities: ps}, nil
		}
	case *c.ScoreQuestion:
		if a.Type == "score" && a.Score != nil && a.Confidence != nil && len(a.Legend) == len(q.Levels) && len(a.Probabilities) == len(q.Levels) {
			ps := make([]float64, len(q.Levels))
			for i, level := range q.Levels {
				k := strconv.Itoa(i)
				p := a.Probabilities[k]
				if p == nil || a.Legend[k] != level {
					return nil, invalid("score levels do not match rubric")
				}
				ps[i] = *p
			}
			return &c.ScoreAnswer{Score: *a.Score, Confidence: *a.Confidence, Probabilities: ps}, nil
		}
	}
	return nil, invalid("missing or mismatched answer fields")
}
