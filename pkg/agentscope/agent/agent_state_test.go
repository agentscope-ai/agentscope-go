package agent

import (
	"encoding/json"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestAgentState_JSONRoundTrip(t *testing.T) {
	state := &AgentState{
		SessionID: "sess-123",
		Summary:   "previous work summary",
		ReplyID:   "reply-456",
		CurIter:   3,
		Context: []*message.Msg{
			message.UserMsg("alice", "hello"),
			message.AssistantMsg("bot", "hi there"),
			message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
				message.ToolCallBlock{
					Type:  "tool_call",
					ID:    "tc1",
					Name:  "search",
					Input: `{"query":"test"}`,
					State: message.ToolCallPending,
				},
			}),
			message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
				message.ToolResultBlock{
					Type:   "tool_result",
					ID:     "tc1",
					Name:   "search",
					Output: "found 3 results",
					State:  message.ToolResultSuccess,
				},
			}),
		},
	}

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}

	var restored AgentState
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	if restored.SessionID != "sess-123" {
		t.Errorf("SessionID = %s, want sess-123", restored.SessionID)
	}
	if restored.Summary != "previous work summary" {
		t.Errorf("Summary = %s", restored.Summary)
	}
	if restored.ReplyID != "reply-456" {
		t.Errorf("ReplyID = %s", restored.ReplyID)
	}
	if restored.CurIter != 3 {
		t.Errorf("CurIter = %d, want 3", restored.CurIter)
	}
	if len(restored.Context) != 4 {
		t.Fatalf("Context len = %d, want 4", len(restored.Context))
	}

	// Verify first msg
	if restored.Context[0].Role != message.RoleUser {
		t.Errorf("Context[0].Role = %s, want user", restored.Context[0].Role)
	}

	// Verify tool call block
	tc, ok := restored.Context[2].Content[0].(message.ToolCallBlock)
	if !ok {
		t.Fatalf("Context[2].Content[0] type = %T, want ToolCallBlock", restored.Context[2].Content[0])
	}
	if tc.Name != "search" {
		t.Errorf("ToolCall.Name = %s, want search", tc.Name)
	}

	// Verify tool result block
	tr, ok := restored.Context[3].Content[0].(message.ToolResultBlock)
	if !ok {
		t.Fatalf("Context[3].Content[0] type = %T, want ToolResultBlock", restored.Context[3].Content[0])
	}
	if tr.Output != "found 3 results" {
		t.Errorf("ToolResult.Output = %v", tr.Output)
	}
}

func TestAgentState_EmptyState(t *testing.T) {
	state := &AgentState{SessionID: "empty"}

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}

	var restored AgentState
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	if restored.SessionID != "empty" {
		t.Errorf("SessionID = %s", restored.SessionID)
	}
	if len(restored.Context) != 0 {
		t.Errorf("Context should be empty, got %d", len(restored.Context))
	}
}

func TestAgentState_WithUsage(t *testing.T) {
	msg := message.AssistantMsg("bot", "reply")
	msg.Usage = &message.Usage{InputTokens: 500, OutputTokens: 100}

	state := &AgentState{
		SessionID: "usage-test",
		Context:   []*message.Msg{msg},
	}

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}

	var restored AgentState
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	if restored.Context[0].Usage == nil {
		t.Fatal("Usage should be preserved")
	}
	if restored.Context[0].Usage.InputTokens != 500 {
		t.Errorf("InputTokens = %d, want 500", restored.Context[0].Usage.InputTokens)
	}
}

func TestAgentState_WithStateOption(t *testing.T) {
	savedState := &AgentState{
		SessionID: "restored-session",
		Summary:   "previous context summary",
		Context: []*message.Msg{
			message.UserMsg("user", "prior message"),
		},
	}

	mock := &mockChatModel{
		responses: []model.ChatResponse{
			{
				Content: []message.ContentBlock{
					message.TextBlock{Type: "text", ID: "t1", Text: "Resumed!"},
				},
				IsLast: true,
			},
		},
	}

	agent := NewUnifiedAgent("bot", "prompt", mock, WithState(savedState))

	if agent.state.SessionID != "restored-session" {
		t.Errorf("SessionID = %s, want restored-session", agent.state.SessionID)
	}
	if agent.state.Summary != "previous context summary" {
		t.Errorf("Summary = %s", agent.state.Summary)
	}
	if len(agent.state.Context) != 1 {
		t.Errorf("Context len = %d, want 1", len(agent.state.Context))
	}
}
