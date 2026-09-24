package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type recoveryToolModel struct{ calls atomic.Int32 }

func (m *recoveryToolModel) Chat(context.Context, []*message.Msg, ...model.CallOption) (*model.ChatResponse, error) {
	if m.calls.Add(1) == 1 {
		return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "call", Name: "hold", Input: `{}`, State: message.ToolCallPending}}}, nil
	}
	return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}}, nil
}
func (*recoveryToolModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (*recoveryToolModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

type recoveryCancelForwarder struct {
	middleware.BaseMiddleware
	done chan struct{}
	once sync.Once
}

func (m *recoveryCancelForwarder) OnReply(ctx context.Context, input middleware.ReplyInput, next middleware.ReplyHandler) <-chan event.Event {
	source := next(ctx, input)
	out := make(chan event.Event)
	go func() {
		defer close(out)
		defer func() {
			go func() {
				for range source {
				}
				m.once.Do(func() { close(m.done) })
			}()
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-source:
				if !ok {
					return
				}
				select {
				case out <- e:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func TestRecoveryRemainsExclusiveUntilCanceledCoreExits(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	hold := tool.NewFunctionTool("hold", "fixture", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) {
		close(entered)
		<-release
		return "old result", nil
	})
	mw := &recoveryCancelForwarder{done: make(chan struct{})}
	a := NewUnifiedAgent("bot", "prompt", &recoveryToolModel{}, WithReplyRecovery(), WithStateSaver(newFakeStateSaver()), WithToolkit(tool.NewToolkit(hold)), WithPermissionContext(permission.NewContext(permission.ModeBypass)), WithMiddlewares(mw))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := a.ReplyStream(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() {
		for range out {
		}
		close(drained)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	cancel()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("outward stream did not close")
	}
	select {
	case <-mw.done:
		t.Fatal("fixture core already completed")
	default:
	}
	if newer, err := a.ReplyStream(ctx, "second"); err == nil {
		for range newer {
		}
		t.Fatal("new reply accepted while prior core still owns agent state")
	}
}

type recoveryHookSaver struct {
	*fakeStateSaver
	hook func(*AgentState) error
}

func (s *recoveryHookSaver) SaveState(ctx context.Context, id string, st *AgentState) error {
	if s.hook != nil {
		if err := s.hook(st); err != nil {
			return err
		}
	}
	return s.fakeStateSaver.SaveState(ctx, id, st)
}

func TestRecoveryResumesPendingToolAtIterationLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	captured := make(chan []byte, 1)
	saver := &recoveryHookSaver{fakeStateSaver: newFakeStateSaver(), hook: func(st *AgentState) error {
		for _, msg := range st.Context {
			if len(msg.GetContentBlocks(message.ContentBlockToolCall)) > 0 {
				raw, err := json.Marshal(st)
				if err != nil {
					return err
				}
				select {
				case captured <- raw:
					cancel()
				default:
				}
				break
			}
		}
		return nil
	}}
	hold := tool.NewFunctionTool("hold", "fixture", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) { return "value", nil })
	a := NewUnifiedAgent("bot", "prompt", &recoveryToolModel{}, WithReplyRecovery(), WithStateSaver(saver), WithToolkit(tool.NewToolkit(hold)), WithPermissionContext(permission.NewContext(permission.ModeBypass)), WithReactConfig(ReactConfig{MaxIters: 1}))
	out, err := a.ReplyStream(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	for range out {
	}
	var saved AgentState
	select {
	case raw := <-captured:
		if err := json.Unmarshal(raw, &saved); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("no model checkpoint")
	}
	var calls atomic.Int32
	hold = tool.NewFunctionTool("hold", "fixture", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) { calls.Add(1); return "value", nil })
	resumed := NewUnifiedAgent("bot", "prompt", &recoveryModel{}, WithReplyRecovery(), WithStateSaver(newFakeStateSaver()), WithState(&saved), WithToolkit(tool.NewToolkit(hold)), WithPermissionContext(permission.NewContext(permission.ModeBypass)), WithReactConfig(ReactConfig{MaxIters: 1}))
	out, err = resumed.ResumeReplyStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for range out {
	}
	if calls.Load() != 1 {
		t.Fatalf("pending tool lost at iteration boundary: calls=%d", calls.Load())
	}
}

func TestRecoveryAcceptedTerminalSaveFailure(t *testing.T) {
	saver := &recoveryHookSaver{fakeStateSaver: newFakeStateSaver(), hook: func(st *AgentState) error {
		if st.ReplyRecovery != nil && !st.ReplyRecovery.Active {
			return errors.New("terminal save failed")
		}
		return nil
	}}
	a := NewUnifiedAgent("bot", "prompt", &recoveryModel{}, WithReplyRecovery(), WithStateSaver(saver))
	if _, err := a.Reply(context.Background(), "hi"); err == nil {
		t.Fatal("failed terminal save returned success")
	}
	st, err := LoadCheckpoint(context.Background(), saver, a.state.SessionID)
	if err != nil || !st.ReplyRecovery.Active {
		t.Fatalf("last durable checkpoint was lost: %+v %v", st, err)
	}
}

func TestRecoveryRevalidatesUnfinishedApprovals(t *testing.T) {
	for _, state := range []message.ToolCallState{message.ToolCallAllowed, message.ToolCallAsking, message.ToolCallPending} {
		t.Run(string(state), func(t *testing.T) {
			var executed atomic.Int32
			hold := tool.NewFunctionTool("hold", "fixture", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) { executed.Add(1); return "value", nil })
			msg := message.AssistantMsg("bot", []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "call", Name: "hold", Input: `{}`, State: state}})
			msg.ID = "r"
			saved := &AgentState{SessionID: "s", ReplyID: "r", Context: []*message.Msg{msg}, ReplyRecovery: &ReplyRecoveryState{Version: 1, ReplyID: "r", Active: true, Budgets: middleware.ReplyBudgetSnapshot{Version: 1}}}
			policy := permission.NewContext(permission.ModeDefault)
			policy.DenyRules["hold"] = []permission.Rule{{ToolName: "hold", Behavior: permission.BehaviorDeny, Source: "current policy"}}
			a := NewUnifiedAgent("bot", "prompt", &recoveryModel{}, WithState(saved), WithReplyRecovery(), WithStateSaver(newFakeStateSaver()), WithToolkit(tool.NewToolkit(hold)), WithPermissionContext(policy))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			out, err := a.ResumeReplyStream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			asked := false
			for e := range out {
				if _, ok := e.(event.RequireUserConfirmEvent); ok {
					asked = true
					cancel()
				}
			}
			if executed.Load() != 0 || asked {
				t.Fatalf("stale approval survived current deny: executed=%d asked=%v", executed.Load(), asked)
			}
			if got := msg.Content[0].(message.ToolCallBlock).State; got != state {
				t.Fatal("resume mutated caller-owned checkpoint")
			}
		})
	}
}
