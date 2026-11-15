package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func emptySchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

// TestOrchestrator_PerToolTimeout proves a blocking tool is bounded by
// DefaultToolTimeout instead of hanging the whole loop.
func TestOrchestrator_PerToolTimeout(t *testing.T) {
	slow := NewFunctionTool("slow", "blocks", emptySchema(),
		func(ctx context.Context, input map[string]any) (any, error) {
			<-ctx.Done() // block until the per-tool timeout cancels ctx
			return nil, ctx.Err()
		})
	o := NewOrchestrator(OrchestratorConfig{
		Toolkit:            NewToolkit(slow),
		DefaultToolTimeout: 50 * time.Millisecond,
	})

	done := make(chan struct{})
	go func() {
		_, _ = o.Execute(context.Background(), message.ToolCallBlock{Name: "slow", Input: "{}"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Execute did not honor DefaultToolTimeout (tool hung the loop)")
	}
}

// TestOrchestrator_ResultCap proves oversized tool output is truncated.
func TestOrchestrator_ResultCap(t *testing.T) {
	big := NewFunctionTool("big", "large output", emptySchema(),
		func(ctx context.Context, input map[string]any) (any, error) {
			return strings.Repeat("x", 10000), nil
		})
	o := NewOrchestrator(OrchestratorConfig{
		Toolkit:            NewToolkit(big),
		MaxToolResultBytes: 100,
	})

	resp, err := o.Execute(context.Background(), message.ToolCallBlock{Name: "big", Input: "{}"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	total := 0
	for _, b := range resp.Content {
		if tb, ok := b.(message.TextBlock); ok {
			total += len(tb.Text)
		}
	}
	if total > 100+200 { // cap + a small truncation-notice allowance
		t.Fatalf("result not capped: %d bytes of text", total)
	}
}
