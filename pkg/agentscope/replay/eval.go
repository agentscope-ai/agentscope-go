package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Scorer evaluates a recorded response against expectations.
// Implementations return a score in [0, 1] where 1 means perfect match.
type Scorer interface {
	Score(ctx context.Context, expected, actual json.RawMessage) (float64, error)
}

// ScorerFunc adapts a plain function to the Scorer interface.
type ScorerFunc func(ctx context.Context, expected, actual json.RawMessage) (float64, error)

func (f ScorerFunc) Score(ctx context.Context, expected, actual json.RawMessage) (float64, error) {
	return f(ctx, expected, actual)
}

// EvalResult holds the outcome of evaluating a single tape entry.
type EvalResult struct {
	Index    int     `json:"index"`
	Score    float64 `json:"score"`
	Pass     bool    `json:"pass"`
	Expected string  `json:"expected,omitempty"`
	Actual   string  `json:"actual,omitempty"`
	Error    string  `json:"error,omitempty"`
}

// EvalReport summarizes evaluation of a full tape.
type EvalReport struct {
	TapePath  string       `json:"tape_path,omitempty"`
	Total     int          `json:"total"`
	Passed    int          `json:"passed"`
	Failed    int          `json:"failed"`
	Errors    int          `json:"errors"`
	MeanScore float64      `json:"mean_score"`
	Results   []EvalResult `json:"results"`
}

// EvalTape evaluates every entry in recorded against the corresponding entry
// in expected using the given scorer. The two tapes must have the same number
// of entries. The threshold (0–1) determines pass/fail per entry.
func EvalTape(ctx context.Context, expected, recorded *Tape, scorer Scorer, threshold float64) (*EvalReport, error) {
	if expected == nil || recorded == nil {
		return nil, fmt.Errorf("eval: expected and recorded tapes must not be nil")
	}
	if scorer == nil {
		return nil, fmt.Errorf("eval: scorer must not be nil")
	}
	if len(expected.Entries) != len(recorded.Entries) {
		return nil, fmt.Errorf("eval: entry count mismatch: expected %d, recorded %d",
			len(expected.Entries), len(recorded.Entries))
	}
	if threshold < 0 {
		threshold = 1.0
	}

	report := &EvalReport{
		Total:   len(expected.Entries),
		Results: make([]EvalResult, 0, len(expected.Entries)),
	}
	var scoreSum float64

	for i := range expected.Entries {
		er := EvalResult{Index: i}

		score, err := scorer.Score(ctx, expected.Entries[i].Response, recorded.Entries[i].Response)
		if err != nil {
			er.Error = err.Error()
			report.Errors++
			report.Results = append(report.Results, er)
			continue
		}

		er.Score = score
		er.Pass = score >= threshold
		if er.Pass {
			report.Passed++
		} else {
			report.Failed++
			er.Expected = string(expected.Entries[i].Response)
			er.Actual = string(recorded.Entries[i].Response)
		}
		scoreSum += score
		report.Results = append(report.Results, er)
	}

	if report.Total > report.Errors {
		report.MeanScore = scoreSum / float64(report.Total-report.Errors)
	}
	return report, nil
}

// AssertTape is a testing helper that loads an expected tape, replays a
// recorded tape against it, and fails the test if any entry falls below
// the threshold. This integrates naturally with `go test`.
func AssertTape(t *testing.T, expected, recorded *Tape, scorer Scorer, threshold float64) {
	t.Helper()

	report, err := EvalTape(context.Background(), expected, recorded, scorer, threshold)
	if err != nil {
		t.Fatalf("EvalTape: %v", err)
	}

	for _, r := range report.Results {
		if r.Error != "" {
			t.Errorf("entry %d: scorer error: %s", r.Index, r.Error)
		} else if !r.Pass {
			t.Errorf("entry %d: score %.3f < threshold %.3f\n  expected: %s\n  actual:   %s",
				r.Index, r.Score, threshold, r.Expected, r.Actual)
		}
	}

	if report.Failed > 0 || report.Errors > 0 {
		t.Errorf("eval summary: %d/%d passed, %d failed, %d errors, mean score %.3f",
			report.Passed, report.Total, report.Failed, report.Errors, report.MeanScore)
	}
}

// ---------- Built-in scorers ----------

// ExactMatchScorer returns 1.0 if the JSON-encoded responses are byte-equal,
// 0.0 otherwise.
var ExactMatchScorer Scorer = ScorerFunc(func(_ context.Context, expected, actual json.RawMessage) (float64, error) {
	if string(expected) == string(actual) {
		return 1.0, nil
	}
	return 0.0, nil
})

// ContainsScorer returns 1.0 if the actual response text contains the
// substring, 0.0 otherwise. The expected tape entry is ignored; use the
// factory function to set the substring.
func ContainsScorer(substring string) Scorer {
	return ScorerFunc(func(_ context.Context, _, actual json.RawMessage) (float64, error) {
		if strings.Contains(string(actual), substring) {
			return 1.0, nil
		}
		return 0.0, nil
	})
}

// JSONFieldScorer extracts a top-level field from both expected and actual
// (assumed to be JSON objects) and compares their string representations.
// Returns 1.0 on match, 0.0 on mismatch. Missing fields compare as empty.
func JSONFieldScorer(field string) Scorer {
	return ScorerFunc(func(_ context.Context, expected, actual json.RawMessage) (float64, error) {
		extract := func(raw json.RawMessage) string {
			var m map[string]json.RawMessage
			if json.Unmarshal(raw, &m) != nil {
				return ""
			}
			return string(m[field])
		}
		if extract(expected) == extract(actual) {
			return 1.0, nil
		}
		return 0.0, nil
	})
}

// TextContainsScorer returns 1.0 if the actual response (treated as a raw
// string or JSON text) contains all of the given substrings (case-insensitive).
func TextContainsScorer(substrings ...string) Scorer {
	return ScorerFunc(func(_ context.Context, _, actual json.RawMessage) (float64, error) {
		lower := strings.ToLower(string(actual))
		for _, s := range substrings {
			if !strings.Contains(lower, strings.ToLower(s)) {
				return 0.0, nil
			}
		}
		return 1.0, nil
	})
}

// CompositeScorer runs multiple scorers and returns their arithmetic mean.
func CompositeScorer(scorers ...Scorer) Scorer {
	return ScorerFunc(func(ctx context.Context, expected, actual json.RawMessage) (float64, error) {
		if len(scorers) == 0 {
			return 0, nil
		}
		var sum float64
		for _, s := range scorers {
			score, err := s.Score(ctx, expected, actual)
			if err != nil {
				return 0, err
			}
			sum += score
		}
		return sum / float64(len(scorers)), nil
	})
}
