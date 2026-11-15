package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/replay"
)

// This example demonstrates the replay eval harness.
// It creates expected and recorded tapes, then evaluates them with
// ExactMatchScorer and ContainsScorer to produce an EvalReport.

func main() {
	fmt.Println("=== Replay Eval Harness Example ===")
	fmt.Println()
	ctx := context.Background()

	// Build the "expected" tape (ground truth).
	expected := replay.NewTape()
	expected.Metadata = map[string]string{"scenario": "qa-eval"}
	expected.Entries = []replay.Entry{
		{
			Index:     0,
			AgentName: "qa-bot",
			ModelName: "gpt-4o-mini",
			Response:  json.RawMessage(`"Go is a statically typed compiled language."`),
			Timestamp: time.Now(),
		},
		{
			Index:     1,
			AgentName: "qa-bot",
			ModelName: "gpt-4o-mini",
			Response:  json.RawMessage(`"The capital of France is Paris."`),
			Timestamp: time.Now(),
		},
		{
			Index:     2,
			AgentName: "qa-bot",
			ModelName: "gpt-4o-mini",
			Response:  json.RawMessage(`"Water boils at 100 degrees Celsius."`),
			Timestamp: time.Now(),
		},
	}

	// Build the "recorded" tape (actual model output to evaluate).
	recorded := replay.NewTape()
	recorded.Entries = []replay.Entry{
		{
			Index:     0,
			AgentName: "qa-bot",
			ModelName: "gpt-4o-mini",
			Response:  json.RawMessage(`"Go is a statically typed compiled language."`), // exact match
			Timestamp: time.Now(),
		},
		{
			Index:     1,
			AgentName: "qa-bot",
			ModelName: "gpt-4o-mini",
			Response:  json.RawMessage(`"The capital of France is Lyon."`), // WRONG
			Timestamp: time.Now(),
		},
		{
			Index:     2,
			AgentName: "qa-bot",
			ModelName: "gpt-4o-mini",
			Response:  json.RawMessage(`"Water boils at 100 degrees Celsius at sea level."`), // close but not exact
			Timestamp: time.Now(),
		},
	}

	// --- Evaluate with ExactMatchScorer (threshold=1.0) ---
	fmt.Println("--- ExactMatchScorer (threshold=1.0) ---")
	report, err := replay.EvalTape(ctx, expected, recorded, replay.ExactMatchScorer, 1.0)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	printReport(report)
	fmt.Println()

	// --- Evaluate with ContainsScorer (checks if "100 degrees" is in output) ---
	fmt.Println("--- ContainsScorer('100 degrees', threshold=1.0) ---")
	report2, err := replay.EvalTape(ctx, expected, recorded, replay.ContainsScorer("100 degrees"), 1.0)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	printReport(report2)
	fmt.Println()

	// --- Evaluate with CompositeScorer (mean of exact + contains) ---
	fmt.Println("--- CompositeScorer(ExactMatch + Contains('Paris'), threshold=0.5) ---")
	composite := replay.CompositeScorer(replay.ExactMatchScorer, replay.ContainsScorer("Paris"))
	report3, err := replay.EvalTape(ctx, expected, recorded, composite, 0.5)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	printReport(report3)

	fmt.Println()
	fmt.Println("=== Done ===")
}

func printReport(r *replay.EvalReport) {
	fmt.Printf("  Total: %d | Passed: %d | Failed: %d | Errors: %d\n",
		r.Total, r.Passed, r.Failed, r.Errors)
	fmt.Printf("  Mean Score: %.3f\n", r.MeanScore)
	for _, res := range r.Results {
		status := "PASS"
		if !res.Pass {
			status = "FAIL"
		}
		if res.Error != "" {
			status = "ERR "
		}
		fmt.Printf("    [%d] %s  score=%.2f", res.Index, status, res.Score)
		if !res.Pass && res.Actual != "" {
			fmt.Printf("  expected=%s  actual=%s", res.Expected, res.Actual)
		}
		fmt.Println()
	}
}
