package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// usageMockModel reports a low byte-estimate token count but high actual
// usage, mimicking CJK/JSON-heavy contexts where len/4 under-counts.
type usageMockModel struct {
	streams atomic.Int32
}

func (m *usageMockModel) Chat(context.Context, []*message.Msg, ...model.CallOption) (*model.ChatResponse, error) {
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.ToolCallBlock{
			Type:  "tool_call",
			ID:    "tc_summary",
			Name:  "generate_structured_output",
			Input: `{"task_overview":"Earlier work","current_state":"In progress","important_discoveries":"None","next_steps":"Continue","context_to_preserve":"Nothing"}`,
			State: message.ToolCallPending,
		}},
		IsLast: true,
		Usage:  &model.ChatUsage{InputTokens: 5, OutputTokens: 5},
	}, nil
}

func (m *usageMockModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	m.streams.Add(1)
	ch := make(chan model.ChatResponse, 1)
	ch <- model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}},
		IsLast:  true,
		Usage:   &model.ChatUsage{InputTokens: 9000, OutputTokens: 2},
	}
	close(ch)
	return ch, nil
}

// CountTokens deliberately under-reports, as byte heuristics do for CJK.
func (m *usageMockModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 10 }

func (m *usageMockModel) ContextSize() int { return 10000 }

// The compression trigger must honor the provider-reported input size even
// when the byte estimate says the context is small.
func TestCompressionTriggerUsesActualUsage(t *testing.T) {
	m := &usageMockModel{}
	a := NewUnifiedAgent("bot", "test", m,
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8}), // threshold: 8000 tokens
	)
	if _, end, _ := collectReply(t, a, "first"); end.FinishedReason != types.ReplyCompleted {
		t.Fatalf("first reply: %+v", end)
	}
	a.mu.Lock()
	if a.lastModelInputTokens != 9000 {
		t.Fatalf("lastModelInputTokens = %d, want 9000", a.lastModelInputTokens)
	}
	a.mu.Unlock()

	if _, end, _ := collectReply(t, a, "second"); end.FinishedReason != types.ReplyCompleted {
		t.Fatalf("second reply: %+v", end)
	}
	a.mu.Lock()
	summary := a.state.Summary
	floor := a.lastModelInputTokens
	a.mu.Unlock()
	if summary == "" {
		t.Fatal("actual usage above the trigger ratio should have driven compaction")
	}
	if floor != 9000 {
		t.Fatalf("post-compaction model call should refresh the usage floor, got %d", floor)
	}
}

// Without actual usage (usage-less providers) the byte estimate alone drives
// the trigger — unchanged behavior.
func TestCompressionTriggerWithoutUsage(t *testing.T) {
	m := &scriptedStream{steps: []streamStep{
		{chunks: []model.ChatResponse{{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}},
			IsLast:  true,
		}}},
	}}
	a := NewUnifiedAgent("bot", "test", m,
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8, ContextSize: 10000}),
	)
	if _, end, _ := collectReply(t, a, "first"); end.FinishedReason != types.ReplyCompleted {
		t.Fatalf("reply: %+v", end)
	}
	a.mu.Lock()
	summary := a.state.Summary
	floor := a.lastModelInputTokens
	a.mu.Unlock()
	if summary != "" {
		t.Fatal("byte estimate below threshold must not compact")
	}
	if floor != 0 {
		t.Fatalf("usage-less response must not set the floor, got %d", floor)
	}
}
