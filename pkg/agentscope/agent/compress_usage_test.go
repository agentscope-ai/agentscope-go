package agent

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func summaryToolCallResponse(usage *model.ChatUsage) *model.ChatResponse {
	return &model.ChatResponse{
		Content: []message.ContentBlock{
			message.ToolCallBlock{
				Type:  "tool_call",
				ID:    "tc_summary",
				Name:  "generate_structured_output",
				Input: `{"task_overview":"T","current_state":"S","important_discoveries":"D","next_steps":"N","context_to_preserve":"P"}`,
				State: message.ToolCallPending,
			},
		},
		IsLast: true,
		Usage:  usage,
	}
}

// Upstream #2433: compression model calls burn tokens; their usage must be
// recorded so the reply loop can account it.
func TestCompressionUsageRecorded(t *testing.T) {
	mock := &compressionMockModel{
		tokenCount:   90,
		contextSize:  100,
		chatResponse: summaryToolCallResponse(&model.ChatUsage{InputTokens: 11, OutputTokens: 7}),
	}
	a := NewUnifiedAgent("t", "sys", mock,
		WithContextConfig(&ContextConfig{ContextSize: 100, TriggerRatio: 0.5, ReserveRatio: 0.1}),
	)
	for i := 0; i < 8; i++ {
		role := message.RoleUser
		if i%2 == 1 {
			role = message.RoleAssistant
		}
		a.state.Context = append(a.state.Context, message.NewMsg("u", role, "message body"))
	}

	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	u := a.takeCompressionUsage()
	if u == nil {
		t.Fatal("compression usage not recorded")
	}
	if u.InputTokens != 11 || u.OutputTokens != 7 {
		t.Errorf("usage = in:%d out:%d, want in:11 out:7", u.InputTokens, u.OutputTokens)
	}
	// Drained: a second take returns nil.
	if a.takeCompressionUsage() != nil {
		t.Error("takeCompressionUsage must clear the accumulator")
	}
}

func TestCompressionUsageNotRecordedWhenNoCompression(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 10, contextSize: 100}
	a := NewUnifiedAgent("t", "sys", mock,
		WithContextConfig(&ContextConfig{ContextSize: 100, TriggerRatio: 0.8}),
	)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	if u := a.takeCompressionUsage(); u != nil {
		t.Errorf("no compression ran, usage should be nil, got %+v", u)
	}
}
