package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// Fresh-window compaction folds the history into the summary; at most the
// trailing message survives (the split keeps it so an in-flight tool call
// never loses its pair).
func TestCompactFreshWindowKeepsNoTail(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 5000, contextSize: 32000}
	a := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{
			TriggerRatio:    0.8,
			ReserveRatio:    0.5, // would keep a large tail under reserve_tail
			CompactStrategy: CompactStrategyFreshWindow,
		}),
	)
	for i := 0; i < 10; i++ {
		a.state.Context = append(a.state.Context,
			message.UserMsg("user", "message"),
			message.AssistantMsg("test", "reply"),
		)
	}
	compressed, err := a.Compact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !compressed {
		t.Fatal("no compaction happened")
	}
	if len(a.state.Context) > 1 {
		t.Errorf("fresh window must reserve at most the trailing message, got %d", len(a.state.Context))
	}
	if a.state.Summary == "" {
		t.Error("summary missing after fresh-window compaction")
	}
}

// Default stays reserve-tail: the recent tail survives compaction.
func TestCompactReserveTailDefault(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 5000, contextSize: 32000}
	a := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8, ReserveRatio: 0.1}),
	)
	for i := 0; i < 10; i++ {
		a.state.Context = append(a.state.Context,
			message.UserMsg("user", "message"),
			message.AssistantMsg("test", "reply"),
		)
	}
	if _, err := a.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(a.state.Context) == 0 {
		t.Error("reserve-tail strategy must keep recent messages")
	}
}

// An over-budget summary is middle-elided: head (overview) and tail survive.
func TestCompactSummaryBudgetEnforced(t *testing.T) {
	huge := strings.Repeat("x", 20000)
	mock := &compressionMockModel{tokenCount: 5000, contextSize: 32000}
	mock.chatResponse = nil // use the default structured-output response
	a := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{
			TriggerRatio:     0.8,
			ReserveRatio:     0.1,
			SummaryMaxTokens: 500, // ~2000 chars
		}),
	)
	// Force a huge summary by swapping the schema payload the mock returns.
	hugeSummary := strings.ReplaceAll(
		`{"task_overview":"HUGE","current_state":"In progress","important_discoveries":"None","next_steps":"Continue","context_to_preserve":"Nothing special"}`,
		"HUGE", huge)
	mock.chatResponse = &model.ChatResponse{
		Content: []message.ContentBlock{
			message.ToolCallBlock{
				Type:  "tool_call",
				ID:    "tc_summary",
				Name:  "generate_structured_output",
				Input: hugeSummary,
				State: message.ToolCallPending,
			},
		},
		IsLast: true,
	}
	for i := 0; i < 10; i++ {
		a.state.Context = append(a.state.Context,
			message.UserMsg("user", "message"),
			message.AssistantMsg("test", "reply"),
		)
	}
	if _, err := a.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.state.Summary, "<<<TRUNCATED:") {
		t.Error("over-budget summary was not elided")
	}
	if len(a.state.Summary) > 500*4+512 {
		t.Errorf("summary still over budget: %d chars", len(a.state.Summary))
	}
}

func TestSummaryMaxTokensDerivation(t *testing.T) {
	cfg := &ContextConfig{}
	if got := summaryMaxTokens(cfg, 128000); got != 4000 {
		t.Errorf("128k window budget = %d, want 4000", got)
	}
	if got := summaryMaxTokens(cfg, 1000); got != 1024 {
		t.Errorf("tiny window budget = %d, want clamp 1024", got)
	}
	if got := summaryMaxTokens(cfg, 1000000); got != 8192 {
		t.Errorf("huge window budget = %d, want clamp 8192", got)
	}
	if got := summaryMaxTokens(&ContextConfig{SummaryMaxTokens: 777}, 128000); got != 777 {
		t.Errorf("explicit budget overridden: %d", got)
	}
	if got := summaryMaxTokens(cfg, 0); got != 4096 {
		t.Errorf("unknown window budget = %d, want 4096", got)
	}
}

// HIGH-1 regression (evaluator): fresh_window + the model-driven
// compress_context tool. The compressing call itself is in flight, so a
// zero-reserve split would orphan the tool_result that lands right after;
// providers reject that with a 400 and kill the reply.
func TestCompactFreshWindowModelDrivenNoOrphan(t *testing.T) {
	// First reply call: compress_context tool call; the compression call
	// (recognized by the generate_structured_output schema in the options)
	// returns the summary payload; then a final text.
	mockModel := &compressDriverMockModel{
		replies: []model.ChatResponse{
			{Content: []message.ContentBlock{
				message.ToolCallBlock{Type: "tool_call", ID: "tc_compact", Name: "compress_context",
					Input: `{}`, State: message.ToolCallPending},
			}, IsLast: true},
			{Content: []message.ContentBlock{
				message.TextBlock{Type: "text", Text: "done"},
			}, IsLast: true},
		},
	}
	a := NewUnifiedAgent("test", "prompt", mockModel,
		WithContextConfig(&ContextConfig{
			TriggerRatio:    0.8,
			CompactStrategy: CompactStrategyFreshWindow,
			ContextSize:     1000,
		}),
		WithAgentDrivenCompression(),
		WithToolkit(tool.NewToolkit()),
		WithReactConfig(ReactConfig{MaxIters: 4}),
	)
	// Give the agent a past sized to sit BETWEEN the agent-driven trigger
	// (0.4*1000=400) and the automatic trigger (0.8*1000=800): 5 pairs x 240
	// chars -> estimate ~600. The turn-start auto-compression must NOT fire
	// (it would eat the history before the tool runs and mask the orphan);
	// the model-driven compress_context call must fire (600 >= 400).
	big := strings.Repeat("old context ", 20)
	for i := 0; i < 5; i++ {
		a.state.Context = append(a.state.Context,
			message.UserMsg("user", big),
			message.AssistantMsg("test", big),
		)
	}

	ch, err := a.ReplyStream(context.Background(), "summarize yourself")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	// The tool must report an actual compression — a no-op would mean the
	// dangerous path (compressing around an in-flight call) never ran.
	sawCompressed := false
	for _, m := range a.state.Context {
		for _, b := range m.Content {
			if tr, ok := b.(message.ToolResultBlock); ok {
				if s, ok := tr.Output.(string); ok && strings.Contains(s, "Context compressed") {
					sawCompressed = true
				}
			}
		}
	}
	if !sawCompressed {
		t.Fatal("compress_context reported no compression; the in-flight path was not exercised")
	}

	// Pairing invariant: every tool_result in context has its call present.
	calls := map[string]bool{}
	for _, m := range a.state.Context {
		for _, b := range m.Content {
			switch blk := b.(type) {
			case message.ToolCallBlock:
				calls[blk.ID] = true
			case message.ToolResultBlock:
				if !calls[blk.ID] {
					t.Fatalf("orphan tool_result %s in context", blk.ID)
				}
			}
		}
	}
	// And the compression actually ran (summary replaced the past).
	if a.state.Summary == "" {
		t.Error("model-driven compression did not produce a summary")
	}
}

// compressDriverMockModel serves both the reply loop (canned responses) and
// the compression summary call (recognized by the generate_structured_output
// tool in the call options), and reports a fixed high token count so the
// agent-driven trigger fires.
type compressDriverMockModel struct {
	replies   []model.ChatResponse
	callCount int
}

func (m *compressDriverMockModel) Chat(_ context.Context, _ []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	o := &model.CallOptions{}
	for _, opt := range opts {
		opt(o)
	}
	for _, ts := range o.Tools {
		if ts.Function.Name == "generate_structured_output" {
			return &model.ChatResponse{
				Content: []message.ContentBlock{
					message.ToolCallBlock{
						Type:  "tool_call",
						ID:    "tc_summary",
						Name:  "generate_structured_output",
						Input: `{"task_overview":"Old task","current_state":"half done","important_discoveries":"none","next_steps":"finish","context_to_preserve":"none"}`,
						State: message.ToolCallPending,
					},
				},
				IsLast: true,
			}, nil
		}
	}
	if m.callCount >= len(m.replies) {
		return &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}},
			IsLast:  true,
		}, nil
	}
	resp := m.replies[m.callCount]
	m.callCount++
	return &resp, nil
}

func (m *compressDriverMockModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (m *compressDriverMockModel) CountTokens(msgs []*message.Msg, _ []model.ToolSchema) int {
	// Content-length estimate, like the real byte heuristics: big history
	// reads big, a compressed summary reads small.
	total := 0
	for _, msg := range msgs {
		for _, b := range msg.Content {
			switch blk := b.(type) {
			case message.TextBlock:
				total += len(blk.Text)
			case message.ToolCallBlock:
				total += len(blk.Input)
			case message.ToolResultBlock:
				if s, ok := blk.Output.(string); ok {
					total += len(s)
				}
			}
		}
	}
	return total / 4
}
