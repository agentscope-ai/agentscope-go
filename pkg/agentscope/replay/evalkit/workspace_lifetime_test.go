package evalkit

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type workspaceHoldModel struct {
	model.ChatModel
	calls      int
	firstPlain bool
}

func (m *workspaceHoldModel) Chat(_ context.Context, msgs []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	m.calls++
	if m.firstPlain && m.calls == 1 {
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "first turn"}}}, nil
	}
	call := m.calls
	if m.firstPlain {
		call--
	}
	if call > 1 {
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}}, nil
	}
	path := ""
	for _, msg := range msgs {
		if msg.Role == message.RoleUser {
			if text := msg.GetTextContent(""); text != nil {
				path = *text
			}
		}
	}
	input, _ := json.Marshal(map[string]string{"path": path})
	return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "hold", Name: "hold_workspace", Input: string(input), State: message.ToolCallPending}}}, nil
}
func (*workspaceHoldModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

func installWorkspaceHold(t *testing.T) (<-chan string, chan<- struct{}, <-chan struct{}) {
	t.Helper()
	entered := make(chan string, 1)
	release := make(chan struct{})
	exited := make(chan struct{})
	prior, existed := LookupToolFactory("hold_workspace")
	RegisterToolFactory("hold_workspace", func() tool.Tool {
		return tool.NewFunctionTool("hold_workspace", "hold test workspace", json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`), func(_ context.Context, input map[string]any) (any, error) {
			path, _ := input["path"].(string)
			entered <- path
			<-release
			if _, err := os.Stat(path); err != nil {
				t.Errorf("workspace deleted while tool still owned it: %v", err)
			}
			close(exited)
			return "done", nil
		})
	})
	t.Cleanup(func() {
		toolRegistryMu.Lock()
		defer toolRegistryMu.Unlock()
		if existed {
			builtinToolFactories["hold_workspace"] = prior
		} else {
			delete(builtinToolFactories, "hold_workspace")
		}
	})
	return entered, release, exited
}
func TestRunTaskWaitsForToolExitBeforeCleanup(t *testing.T) {
	entered, release, exited := installWorkspaceHold(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan TaskResult, 1)
	go func() {
		done <- (&Runner{}).RunTask(ctx, &TaskSpec{ID: "hold", Input: "{workspace}", Tools: []string{"hold_workspace"}, Scorer: ScorerSpec{Ref: "budget"}}, &workspaceHoldModel{})
	}()
	var path string
	select {
	case path = <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("tool did not start")
	}
	cancel()
	// Give the event forwarder time to close. The task must retain ownership.
	select {
	case res := <-done:
		t.Error("task returned before tool exit")
		done <- res
	case <-time.After(30 * time.Millisecond):
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("workspace removed before tool release")
	}
	close(release)
	<-exited
	select {
	case res := <-done:
		if res.Pass || res.Error == "" {
			t.Fatalf("canceled task=%+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("task did not finish")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("workspace not cleaned: %v", err)
	}
}

func waitWorkspaceRemoved(t *testing.T, path string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("workspace not released: %s", path)
		}
	}
}
func TestRunTaskLaterTurnWaitsForItsOwnToolExit(t *testing.T) {
	entered, release, exited := installWorkspaceHold(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan TaskResult, 1)
	go func() {
		done <- (&Runner{}).RunTask(ctx, &TaskSpec{ID: "later-hold", Input: "first", Turns: []string{"{workspace}"}, Tools: []string{"hold_workspace"}, Scorer: ScorerSpec{Ref: "budget"}}, &workspaceHoldModel{firstPlain: true})
	}()
	var path string
	select {
	case path = <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("second-turn tool did not start")
	}
	cancel()
	select {
	case res := <-done:
		t.Error("first-turn completion released second-turn workspace")
		done <- res
	case <-time.After(30 * time.Millisecond):
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("second-turn workspace deleted")
	}
	close(release)
	<-exited
	select {
	case res := <-done:
		if res.Pass || res.Error == "" {
			t.Fatalf("canceled result=%+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second turn did not finish")
	}
	waitWorkspaceRemoved(t, path)
}

func (m workspaceHoldModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
