package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/replay"
)

// This example demonstrates the deterministic replay system.
// It shows how to:
// 1. Create a Tape with recorded model call entries
// 2. Save the tape to disk via FileStore
// 3. Load it back and verify contents
// 4. Use the Recorder and Replayer middleware constructors
//
// In a real workflow, the Recorder middleware would intercept live model calls
// and save them. Later, the Replayer would replay the exact same responses
// for deterministic testing.

func main() {
	fmt.Println("=== Deterministic Replay Example ===")
	fmt.Println()

	// Step 1: Create a tape with simulated model call entries.
	tape := replay.NewTape()
	tape.Metadata = map[string]string{
		"scenario": "greeting-test",
		"created":  time.Now().Format(time.RFC3339),
	}

	// Simulate three recorded interactions (as if a Recorder captured them).
	entries := []replay.Entry{
		{
			Index:      0,
			AgentName:  "greeter",
			ModelName:  "gpt-4o-mini",
			Messages:   json.RawMessage(`[{"role":"user","content":"Hello!"}]`),
			Response:   json.RawMessage(`{"content":[{"type":"text","text":"Hi there! How can I help you?"}],"id":"resp-001"}`),
			Timestamp:  time.Now().Add(-2 * time.Minute),
			DurationMs: 150,
		},
		{
			Index:      1,
			AgentName:  "greeter",
			ModelName:  "gpt-4o-mini",
			Messages:   json.RawMessage(`[{"role":"user","content":"What is Go?"}]`),
			Response:   json.RawMessage(`{"content":[{"type":"text","text":"Go is a statically typed, compiled language designed at Google."}],"id":"resp-002"}`),
			Timestamp:  time.Now().Add(-1 * time.Minute),
			DurationMs: 230,
		},
		{
			Index:      2,
			AgentName:  "greeter",
			ModelName:  "gpt-4o-mini",
			Messages:   json.RawMessage(`[{"role":"user","content":"Thanks!"}]`),
			Response:   json.RawMessage(`{"content":[{"type":"text","text":"You are welcome!"}],"id":"resp-003"}`),
			Timestamp:  time.Now(),
			DurationMs: 95,
		},
	}
	tape.Entries = entries

	fmt.Printf("Created tape with %d entries (version=%s)\n", len(tape.Entries), tape.Version)
	fmt.Printf("Metadata: %v\n", tape.Metadata)
	fmt.Println()

	// Step 2: Save the tape to a temporary directory via FileStore.
	tmpDir := filepath.Join(os.TempDir(), "agentscope-replay-example")
	store, err := replay.NewFileStore(tmpDir)
	if err != nil {
		fmt.Println("Error creating FileStore:", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	if err := store.Save(ctx, "greeting-test", tape); err != nil {
		fmt.Println("Error saving tape:", err)
		return
	}
	fmt.Printf("Saved tape to: %s/greeting-test.json\n", tmpDir)

	// Step 3: Load the tape back and verify.
	loaded, err := store.Load(ctx, "greeting-test")
	if err != nil {
		fmt.Println("Error loading tape:", err)
		return
	}
	fmt.Printf("Loaded tape: version=%s, entries=%d\n", loaded.Version, len(loaded.Entries))
	fmt.Println()

	// List all tapes in the store.
	names, err := store.List(ctx)
	if err != nil {
		fmt.Println("Error listing tapes:", err)
		return
	}
	fmt.Printf("Tapes in store: %v\n", names)
	fmt.Println()

	// Print each entry to show what was recorded.
	fmt.Println("--- Tape Contents ---")
	for i := range loaded.Entries {
		e := &loaded.Entries[i]
		fmt.Printf("  [%d] agent=%s model=%s duration=%dms\n",
			e.Index, e.AgentName, e.ModelName, e.DurationMs)
		fmt.Printf("      messages: %s\n", string(e.Messages))
		fmt.Printf("      response: %s\n", string(e.Response))
	}
	fmt.Println()

	// Step 4: Show the Recorder and Replayer constructors.
	// In production, these would be attached as middleware to agent pipelines.
	recorder := replay.NewRecorder()
	fmt.Printf("Recorder created (tape entries so far: %d)\n", len(recorder.Tape().Entries))

	replayer := replay.NewReplayer(loaded)
	_ = replayer // Would be used as middleware: agent.WithMiddlewares(replayer)
	fmt.Printf("Replayer created with %d pre-recorded entries\n", len(loaded.Entries))
	fmt.Println()

	// Verify round-trip: original and loaded tapes should match.
	origJSON, _ := json.Marshal(tape.Entries)
	loadJSON, _ := json.Marshal(loaded.Entries)
	if bytes.Equal(origJSON, loadJSON) {
		fmt.Println("SUCCESS: Round-trip save/load produces identical tape entries.")
	} else {
		fmt.Println("MISMATCH: Tape entries differ after save/load!")
	}
}
