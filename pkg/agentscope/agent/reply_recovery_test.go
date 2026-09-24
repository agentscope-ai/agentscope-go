package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

type recoveryModel struct{ choices []string }

func (m *recoveryModel) Chat(_ context.Context, _ []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	var o model.CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	choice := ""
	if o.ToolChoice != nil {
		choice = o.ToolChoice.Mode
	}
	m.choices = append(m.choices, choice)
	return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, Usage: &model.ChatUsage{InputTokens: 2}, IsLast: true}, nil
}
func (m *recoveryModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (m *recoveryModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }
func TestResumePreservesBudgetAndNewReplyResets(t *testing.T) {
	budget := middleware.NewReplyBudgetControl(10)
	saver := newFakeStateSaver()
	m := &recoveryModel{}
	state := &AgentState{SessionID: "session", ReplyID: "logical", Context: []*message.Msg{message.UserMsg("user", "hi")}, ReplyRecovery: &ReplyRecoveryState{Version: 1, ReplyID: "logical", Active: true, Budgets: middleware.ReplyBudgetSnapshot{Version: 1, Counters: map[string]middleware.ReplyBudgetCounter{budget.Key(): {UsedTokens: 10}}}}}
	a := NewUnifiedAgent("bot", "prompt", m, WithState(state), WithStateSaver(saver), WithReplyRecovery(), WithMiddlewares(budget))
	events, err := a.ResumeReplyStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for e := range events {
		if end, ok := e.(event.ReplyEndEvent); ok && end.Error != nil {
			t.Fatal(end.Error)
		}
	}
	saved, err := LoadCheckpoint(context.Background(), saver, "session")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.choices) != 1 || m.choices[0] != "none" || saved.ReplyID != "logical" || saved.ReplyRecovery.Active || saved.ReplyRecovery.Budgets.Counters[budget.Key()].UsedTokens != 12 {
		t.Fatalf("resume state wrong: choices=%v state=%+v", m.choices, saved.ReplyRecovery)
	}
	if _, err := a.ResumeReplyStream(context.Background()); err == nil {
		t.Fatal("completed reply resumed")
	}
	if _, err := a.Reply(context.Background(), "new question"); err != nil {
		t.Fatal(err)
	}
	if len(m.choices) != 2 || m.choices[1] == "none" {
		t.Fatalf("new reply inherited budget: %v", m.choices)
	}
}

type failingRecoverySaver struct{ *fakeStateSaver }

func (s failingRecoverySaver) SaveState(context.Context, string, *AgentState) error {
	return errors.New("fixture disk failure")
}
func TestRecoverySaveFailureCannotBeSwallowed(t *testing.T) {
	m := &recoveryModel{}
	a := NewUnifiedAgent("bot", "prompt", m, WithReplyRecovery(), WithStateSaver(failingRecoverySaver{newFakeStateSaver()}), WithMiddlewares(&swallowEndMiddleware{maxSwallow: -1}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	events, err := a.ReplyStream(ctx, "hi")
	if err != nil {
		t.Fatal(err)
	}
	sawError := false
	for e := range events {
		if end, ok := e.(event.ReplyEndEvent); ok && end.Error != nil && strings.Contains(end.Error.Message, "disk failure") {
			sawError = true
		}
	}
	if !sawError || ctx.Err() != nil || len(m.choices) > 0 {
		t.Fatalf("save failure lost or continued: error=%v ctx=%v calls=%d", sawError, ctx.Err(), len(m.choices))
	}
}
func TestResumeRejectsLegacyCorruptAndFutureState(t *testing.T) {
	for _, snapshot := range []*ReplyRecoveryState{nil, {Version: 2, Active: true}, {Version: 1, Active: true, ReplyID: "other"}, {Version: 1, Active: true, ReplyID: "r", Budgets: middleware.ReplyBudgetSnapshot{Version: 2}}} {
		a := NewUnifiedAgent("bot", "prompt", &recoveryModel{}, WithReplyRecovery(), WithStateSaver(newFakeStateSaver()), WithState(&AgentState{SessionID: "s", ReplyID: "r", ReplyRecovery: snapshot}))
		if _, err := a.ResumeReplyStream(context.Background()); err == nil {
			t.Fatal("invalid resume accepted")
		}
	}
}

func TestCheckpointSchemaAndInvalidRecoveryBudget(t *testing.T) {
	saver := newFakeStateSaver()
	a := NewUnifiedAgent("bot", "prompt", &recoveryModel{}, WithStateSaver(saver))
	if err := a.SaveCheckpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadCheckpoint(context.Background(), saver, a.state.SessionID)
	if err != nil || saved.SchemaVersion != 1 || saved.ReplyRecovery != nil {
		t.Fatalf("legacy checkpoint changed format: %+v, %v", saved, err)
	}
	budget := middleware.NewReplyBudgetControl(10)
	budget.InputTokenWeight = -1 // Invalid observed counters must not be persisted.
	a = NewUnifiedAgent("bot", "prompt", &recoveryModel{}, WithStateSaver(saver), WithReplyRecovery(), WithMiddlewares(budget))
	if _, err := a.Reply(context.Background(), "hi"); err == nil {
		t.Fatal("invalid recovery counters were saved successfully")
	}
}
