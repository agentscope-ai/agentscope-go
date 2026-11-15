package model

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// twoTryModel fails the first strategy (no structured result) and succeeds
// on the second, reporting usage on both attempts.
type twoTryModel struct {
	calls int64
}

func (m *twoTryModel) Chat(_ context.Context, _ []*message.Msg, _ ...CallOption) (*ChatResponse, error) {
	n := atomic.AddInt64(&m.calls, 1)
	usage := &ChatUsage{InputTokens: 10, OutputTokens: int(n) * 5}
	if n == 1 {
		// Valid response but no structured tool call → strategy falls through.
		return &ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "prose"}},
			IsLast:  true, Usage: usage,
		}, nil
	}
	return &ChatResponse{
		Content: []message.ContentBlock{message.ToolCallBlock{
			Type: "tool_call", ID: "so1", Name: structuredOutputToolName,
			Input: `{"answer":"42"}`, State: message.ToolCallPending,
		}},
		IsLast: true, Usage: usage,
	}, nil
}

func (m *twoTryModel) ChatStream(context.Context, []*message.Msg, ...CallOption) (<-chan ChatResponse, error) {
	return nil, ErrStreamNotSupported
}
func (m *twoTryModel) CountTokens([]*message.Msg, []ToolSchema) int { return 1 }

// Upstream #2433: every strategy attempt burns tokens; the accumulated usage
// must be reported even though only the last attempt produced the result.
func TestGenerateStructuredOutputWithUsageAccumulates(t *testing.T) {
	m := &twoTryModel{}
	result, usage, err := GenerateStructuredOutputWithUsage(context.Background(), m,
		[]*message.Msg{message.UserMsg("u", "q")}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(result) == 0 {
		t.Fatal("no structured result")
	}
	if usage == nil {
		t.Fatal("usage missing")
	}
	if usage.InputTokens != 20 || usage.OutputTokens != 15 {
		t.Errorf("usage = in:%d out:%d, want accumulated in:20 out:15 (5+10)", usage.InputTokens, usage.OutputTokens)
	}
}

func TestGenerateStructuredOutputUnchangedSignature(t *testing.T) {
	m := &twoTryModel{}
	result, err := GenerateStructuredOutput(context.Background(), m,
		[]*message.Msg{message.UserMsg("u", "q")}, nil)
	if err != nil || len(result) == 0 {
		t.Fatalf("result=%s err=%v", result, err)
	}
}
