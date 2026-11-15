package evalkit

import (
	"context"
	"fmt"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
)

// TaskOutcome carries everything observable about one task run for scoring.
type TaskOutcome struct {
	ErrorType string // structured reply error classification, when available
	// Workspace is borrowed by the scorer while Score runs. Do not retain it
	// or access it after returning; the runner removes it after scoring.
	Workspace         string
	FinalText         string
	Trajectory        []string // tool names in call order
	Iters             int      // model calls observed
	InputTokens       int
	OutputTokens      int
	CacheCreateTokens int
	CacheReadTokens   int
	CostUSD           float64 // populated when the runner can price the model
	CostPriced        bool    // true when CostUSD is a real price (not unknown)
	Events            []event.Event
	Error             string // non-empty when the reply ended with an error
}

// Scorer grades one task outcome in [0,1]; >= 1.0 means pass for
// threshold-1 scorers.
type Scorer interface {
	Score(ctx context.Context, spec *TaskSpec, out *TaskOutcome) (float64, error)
}

// BuildScorer resolves a ScorerSpec into a Scorer.
func BuildScorer(spec *ScorerSpec) (Scorer, error) {
	switch spec.Ref {
	case "", "contains":
		return containsScorer{expect: spec.Expect}, nil
	case "json_field":
		return jsonFieldScorer{field: spec.Field, expect: spec.Expect, source: spec.Source}, nil
	case "text_contains":
		return textContainsScorer{items: spec.Items}, nil
	case "trajectory":
		mode := spec.Mode
		if mode == "" {
			mode = "subsequence"
		}
		return trajectoryScorer{items: spec.Items, mode: mode}, nil
	case "budget":
		return budgetScorer{}, nil
	default:
		return nil, fmt.Errorf("evalkit: unknown scorer ref %q", spec.Ref)
	}
}

type containsScorer struct{ expect string }

func (s containsScorer) Score(_ context.Context, _ *TaskSpec, out *TaskOutcome) (float64, error) {
	if s.expect == "" {
		return 0, fmt.Errorf("contains scorer requires expect")
	}
	if strings.Contains(out.FinalText, s.expect) {
		return 1, nil
	}
	return 0, nil
}

type jsonFieldScorer struct{ field, expect, source string }

func (s jsonFieldScorer) Score(_ context.Context, _ *TaskSpec, out *TaskOutcome) (float64, error) {
	if s.field == "" {
		return 0, fmt.Errorf("json_field scorer requires field")
	}
	text := out.FinalText
	start := strings.Index(text, "{")
	if start < 0 {
		return 0, nil
	}
	// crude first-object extraction; tasks that emit multiple JSON objects
	// should use a custom scorer
	depth, inStr, esc := 0, false, false
	end := -1
	for i := start; i < len(text); i++ {
		c := text[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return 0, nil
	}
	obj := text[start : end+1]
	if strings.Contains(obj, fmt.Sprintf("%q", s.field)) &&
		(s.expect == "" || strings.Contains(obj, s.expect)) {
		return 1, nil
	}
	return 0, nil
}

type textContainsScorer struct{ items []string }

func (s textContainsScorer) Score(_ context.Context, _ *TaskSpec, out *TaskOutcome) (float64, error) {
	if len(s.items) == 0 {
		return 0, fmt.Errorf("text_contains scorer requires items")
	}
	hit := 0
	for _, it := range s.items {
		if strings.Contains(out.FinalText, it) {
			hit++
		}
	}
	return float64(hit) / float64(len(s.items)), nil
}

// trajectoryScorer checks the tool-call sequence. Modes:
//   - exact: trajectory must equal items exactly
//   - subsequence: items must appear in order (extras allowed)
type trajectoryScorer struct {
	items []string
	mode  string
}

func (s trajectoryScorer) Score(_ context.Context, _ *TaskSpec, out *TaskOutcome) (float64, error) {
	if len(s.items) == 0 {
		return 0, fmt.Errorf("trajectory scorer requires items")
	}
	if s.mode == "exact" {
		if len(out.Trajectory) != len(s.items) {
			return 0, nil
		}
		for i := range s.items {
			if out.Trajectory[i] != s.items[i] {
				return 0, nil
			}
		}
		return 1, nil
	}
	// subsequence
	idx := 0
	for _, name := range out.Trajectory {
		if idx < len(s.items) && name == s.items[idx] {
			idx++
		}
	}
	if idx == len(s.items) {
		return 1, nil
	}
	return float64(idx) / float64(len(s.items)), nil
}

// budgetScorer passes when the run stayed inside the task budget bounds.
type budgetScorer struct{}

func (s budgetScorer) Score(_ context.Context, spec *TaskSpec, out *TaskOutcome) (float64, error) {
	b := spec.Budget
	// Multi-turn tasks get the per-reply iteration budget on every turn
	// (runtime enforcement is per reply; the scorer must match, HARNESS
	// review M5).
	iterBudget := b.MaxIters * (1 + len(spec.Turns))
	if b.MaxIters > 0 && out.Iters > iterBudget {
		return 0, nil
	}
	if b.MaxInTokens > 0 && out.InputTokens > b.MaxInTokens {
		return 0, nil
	}
	if b.MaxOutTokens > 0 && out.OutputTokens > b.MaxOutTokens {
		return 0, nil
	}
	if b.MaxCostUSD > 0 {
		// A declared cost budget must not silently pass when the runner
		// cannot price the model — surface the misconfiguration instead
		// (HARNESS review L-2).
		if !out.CostPriced {
			return 0, fmt.Errorf("budget scorer: max_cost_usd declared but the runner cannot price the model (set Runner.ModelName to a model with a known price)")
		}
		if out.CostUSD > b.MaxCostUSD {
			return 0, nil
		}
	}
	return 1, nil
}
