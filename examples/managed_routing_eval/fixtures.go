package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
)

// Fixture answers/categories are intentional lookup tables. They verify the
// pipeline and arithmetic, not generalization, language quality or savings.
type fixtureClassifier struct{ categories map[string]string }

func (*fixtureClassifier) Target() (string, string) { return "fake://classifier", "routing-fixture-v1" }
func (f *fixtureClassifier) Classify(ctx context.Context, r classifier.Request) (*classifier.Response, error) {
	return f.ClassifyWithDispatch(ctx, r, classifier.DispatchLimits{}, nil)
}
func (f *fixtureClassifier) ClassifyWithDispatch(ctx context.Context, r classifier.Request, limits classifier.DispatchLimits, begin func(classifier.AttemptKind) error) (*classifier.Response, error) {
	var text string
	if err := json.Unmarshal(r.State, &text); err != nil {
		return nil, err
	}
	choice := f.categories[text]
	if choice == "" {
		choice = "other"
	}
	probabilities := map[string]float64{"extract": 0, "classify": 0, "rewrite": 0, "other": 0}
	probabilities[choice] = 1
	fake := classifier.NewFake(&classifier.FakeConfig{Response: classifier.Response{Model: "routing-fixture-v1", Answers: map[string]classifier.Answer{"category": &classifier.ChoiceAnswer{Choice: choice, Confidence: 1, Probabilities: probabilities}}}})
	return fake.ClassifyWithDispatch(ctx, r, limits, begin)
}

func fixtureServer(samples []sample, cheap bool) *httptest.Server {
	answers := map[string]sample{}
	for i := range samples {
		s := &samples[i]
		answers[s.Input] = *s
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Messages) != 1 {
			http.Error(w, "invalid fixture input", 400)
			return
		}
		s, ok := answers[request.Messages[0].Content]
		if !ok {
			http.Error(w, "unapproved input", 403)
			return
		}
		answer := s.Answer
		if cheap && s.Category == "rewrite" {
			answer = "wrong fixture output"
		}
		model := "baseline-fixture-v1"
		if cheap {
			model = "candidate-fixture-v1"
		}
		// Each cheap extraction fails once per task (the baseline is an explicit
		// fallback); failed usage is deliberately absent, so price remains unknown.
		if cheap && s.Category == "extract" {
			w.WriteHeader(529)
			fmt.Fprint(w, `{"error":"fixture overload"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": model, "choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": answer}}}, "usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 10}})
	}))
}
