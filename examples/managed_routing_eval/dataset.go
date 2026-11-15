package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/replay/evalkit"
)

//go:embed dataset.json
var datasetJSON []byte

type sample struct {
	ID, Category, Language, Input, Answer string
	Required, Forbidden                   []string
	MaxRunes                              int
}

type corpus struct {
	Version            string
	Tuning, Acceptance []sample
}

func loadSamples(split string) (string, []sample, error) {
	var c corpus
	if err := json.Unmarshal(datasetJSON, &c); err != nil {
		return "", nil, err
	}
	switch split {
	case "tuning":
		return c.Version, c.Tuning, nil
	case "acceptance":
		return c.Version, c.Acceptance, nil
	default:
		return "", nil, fmt.Errorf("split must be tuning or acceptance")
	}
}

// Independent deterministic rubric; Jev never grades completions. The existing
// evalkit Scorer contract keeps load execution and grading separate.
type rubricScorer struct{}

func (rubricScorer) Score(_ context.Context, spec *evalkit.TaskSpec, out *evalkit.TaskOutcome) (float64, error) {
	var gold sample
	if err := json.Unmarshal([]byte(spec.Scorer.Expect), &gold); err != nil {
		return 0, err
	}
	text := strings.TrimSpace(out.FinalText)
	switch gold.Category {
	case "extract":
		var want, got map[string]string
		if err := json.Unmarshal([]byte(gold.Answer), &want); err != nil {
			return 0, err
		}
		if json.Unmarshal([]byte(text), &got) != nil || len(got) != len(want) {
			return 0, nil
		}
		for k, v := range want {
			if got[k] != v {
				return 0, nil
			}
		}
	case "classify":
		if text != gold.Answer {
			return 0, nil
		}
	case "rewrite":
		if text == "" || utf8.RuneCountInString(text) > gold.MaxRunes {
			return 0, nil
		}
		for _, word := range gold.Required {
			if !strings.Contains(text, word) {
				return 0, nil
			}
		}
		for _, word := range gold.Forbidden {
			if strings.Contains(text, word) {
				return 0, nil
			}
		}
	default:
		return 0, fmt.Errorf("unknown rubric category")
	}
	return 1, nil
}
