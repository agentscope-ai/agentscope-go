// An offline classifier fixture. No credentials or network access are needed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
)

// Both implementations below run locally without credentials or network calls.
// A purely local classifier implements the same contract without a dispatch.
// Provider adapters instead record one physical attempt at their send boundary.
type localClassifier struct{}

func (localClassifier) Classify(ctx context.Context, req classifier.Request) (*classifier.Response, error) {
	out := &classifier.Response{OperationID: req.OperationID}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if err := classifier.ValidateRequest(req); err != nil {
		return out, err
	}
	out.Model = "constant-local-v1"
	out.Answers = map[string]classifier.Answer{"positive": &classifier.BinaryAnswer{Probability: 1}}
	return out, classifier.ValidateResponse(req, out)
}

func demonstrateLocalClassifier() {
	var c classifier.Classifier = localClassifier{}
	req := classifier.Request{OperationID: "local-1", State: json.RawMessage(`"example text"`), Questions: map[string]classifier.Question{
		"positive": &classifier.BinaryQuestion{Instructions: "Does the local constant classifier return true?"},
	}}
	out, err := c.Classify(context.Background(), req)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("probability=%.1f attempts=%d\n", out.Answers["positive"].(*classifier.BinaryAnswer).Probability, len(out.Attempts))
}

func main() {
	demonstrateLocalClassifier()

	req := classifier.Request{OperationID: "example-1", State: json.RawMessage(`{"task":"Summarize this paragraph"}`), Questions: map[string]classifier.Question{
		"is_summary": &classifier.BinaryQuestion{Instructions: "Is this a summarization task?"},
		"category":   &classifier.ChoiceQuestion{Instructions: "Choose the task category.", Choices: map[string]string{"summary": "Summarization", "code": "Programming"}},
		"complexity": &classifier.ScoreQuestion{Instructions: "Rate complexity.", Levels: []string{"Simple", "Moderate", "Complex"}},
	}}
	fixture := classifier.Response{Model: "offline-fixture-v1", Answers: map[string]classifier.Answer{
		"is_summary": &classifier.BinaryAnswer{Probability: 0.95},
		"category":   &classifier.ChoiceAnswer{Choice: "summary", Confidence: 0.9, Probabilities: map[string]float64{"summary": 0.95, "code": 0.05}},
		"complexity": &classifier.ScoreAnswer{Score: 0.3, Confidence: 0.8, Probabilities: []float64{0.7, 0.3, 0}},
	}}
	fake := classifier.NewFake(&classifier.FakeConfig{Response: fixture})
	result, err := fake.Classify(context.Background(), req)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("category=%s score=%.1f attempts=%d kind=%s usage_unknown=%t\n",
		result.Answers["category"].(*classifier.ChoiceAnswer).Choice,
		result.Answers["complexity"].(*classifier.ScoreAnswer).Score,
		len(result.Attempts), result.Attempts[0].Kind, result.Attempts[0].Usage == nil)

	// A dispatched failure still carries accounting; never use its answers.
	failure := errors.New("fixture transport failure")
	result, err = classifier.NewFake(&classifier.FakeConfig{Error: failure}).Classify(context.Background(), req)
	fmt.Printf("failure=%t attempts=%d actual_model_unknown=%t\n", errors.Is(err, failure), len(result.Attempts), result.Model == "")

	// Synchronize cancellation after dispatch so this example has one attempt.
	waiting := classifier.NewFake(&classifier.FakeConfig{WaitForCancel: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); result, err = waiting.Classify(ctx, req) }()
	<-waiting.Started()
	cancel()
	<-done
	fmt.Printf("canceled=%t attempts=%d kind=%s\n", errors.Is(err, context.Canceled), len(result.Attempts), result.Attempts[0].Kind)
}
