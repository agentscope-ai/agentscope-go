package agent

import (
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// TestBatchToolCalls_HITLForcedSequential proves that a concurrency-safe tool
// which would block on human confirmation is kept out of concurrent batches (so
// concurrent goroutines never contend on the single confirm/external channel).
func TestBatchToolCalls_HITLForcedSequential(t *testing.T) {
	tk := tool.NewToolkit(tool.ReadTool(), tool.GlobTool())
	calls := []message.ToolCallBlock{{Name: "Read", ID: "1"}, {Name: "Glob", ID: "2"}}

	// No HITL predicate: both are concurrency-safe → one concurrent batch of 2.
	base := batchToolCalls(calls, tk, nil)
	if len(base) != 1 || !base[0].concurrent || len(base[0].calls) != 2 {
		t.Fatalf("expected one concurrent batch of 2 without HITL, got %+v", base)
	}

	// Mark Glob as HITL-blocking → it must not be in any concurrent batch.
	blocks := func(tc *message.ToolCallBlock) bool { return tc.Name == "Glob" }
	got := batchToolCalls(calls, tk, blocks)
	for _, b := range got {
		if !b.concurrent {
			continue
		}
		for _, c := range b.calls {
			if c.Name == "Glob" {
				t.Errorf("HITL-blocking tool %q was placed in a concurrent batch", c.Name)
			}
		}
	}
}
