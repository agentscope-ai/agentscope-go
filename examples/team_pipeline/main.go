// A fully offline team: the host confirms a specialist's real fixture-file
// read, and the leader asks that same specialist a follow-up in the same run.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/pipeline"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type scriptedModel struct {
	respond func([]*message.Msg) *model.ChatResponse
}

func (m *scriptedModel) Chat(_ context.Context, msgs []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	return m.respond(msgs), nil
}
func (*scriptedModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (*scriptedModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 0 }
func answer(text string) *model.ChatResponse {
	return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: text}}}
}
func invoke(id, name, input string) *model.ChatResponse {
	return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: id, Name: name, Input: input, State: message.ToolCallPending}}}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	dir, err := os.MkdirTemp("", "agentscope-team-example-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "fixture.txt")
	if err := os.WriteFile(path, []byte("page_size=42"), 0600); err != nil {
		return err
	}
	member := pipeline.TeamMember{Name: "specialist", Description: "Inspects the local example fixture", Factory: func(context.Context) (*agent.UnifiedAgent, error) {
		read := tool.NewFunctionTool("inspect_fixture", "Read the example-owned fixture", nil, func(context.Context, map[string]any) (any, error) { b, err := os.ReadFile(path); return string(b), err })
		n := 0
		m := &scriptedModel{respond: func(msgs []*message.Msg) *model.ChatResponse {
			n++
			if n == 1 {
				return invoke("read-fixture", "inspect_fixture", `{}`)
			}
			inputs := 0
			found := false
			for _, msg := range msgs {
				if msg.Role == message.RoleUser {
					inputs++
				}
				for _, block := range msg.Content {
					if tr, ok := block.(message.ToolResultBlock); ok && strings.Contains(tr.GetOutputText(), "page_size=42") {
						found = true
					}
				}
			}
			return answer(fmt.Sprintf("Fixture verified=%v; member input count=%d", found, inputs))
		}}
		return agent.NewUnifiedAgent("specialist", "Inspect only the example fixture.", m, agent.WithToolkit(tool.NewToolkit(read)), agent.WithPermissionContext(permission.NewContext(permission.ModeDefault)), agent.WithModelStreaming(false)), nil
	}}
	leader := func(_ context.Context, assign tool.Tool) (*agent.UnifiedAgent, error) {
		tk, err := pipeline.NewTeamLeaderToolkit(assign)
		if err != nil {
			return nil, err
		}
		pctx := permission.NewContext(permission.ModeDefault)
		pctx.AllowRules[pipeline.TeamAssignToolName] = []permission.Rule{{ToolName: pipeline.TeamAssignToolName, Behavior: permission.BehaviorAllow, Source: "example"}}
		n := 0
		m := &scriptedModel{respond: func(msgs []*message.Msg) *model.ChatResponse {
			n++
			switch n {
			case 1:
				return invoke("inspect", pipeline.TeamAssignToolName, `{"member":"specialist","prompt":"Inspect the fixture."}`)
			case 2:
				return invoke("followup", pipeline.TeamAssignToolName, `{"member":"specialist","prompt":"Confirm the value from your earlier inspection."}`)
			default:
				for i := len(msgs) - 1; i >= 0; i-- {
					for j := len(msgs[i].Content) - 1; j >= 0; j-- {
						if tr, ok := msgs[i].Content[j].(message.ToolResultBlock); ok && tr.ID == "followup" {
							return answer("Leader received: " + tr.GetOutputText())
						}
					}
				}
				return answer("Missing follow-up result")
			}
		}}
		return agent.NewUnifiedAgent("leader", "Delegate fixture inspection.", m, agent.WithToolkit(tk), agent.WithPermissionContext(pctx), agent.WithModelStreaming(false)), nil
	}
	team, err := pipeline.NewTeamPipeline(leader, member)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := team.Start(ctx, "Inspect and follow up.")
	if err != nil {
		return err
	}
	defer func() { r.Cancel(); _ = r.Wait() }()
	names := map[string]string{}
	for ev := range r.Events() {
		switch e := ev.(type) {
		case event.ReplyStartEvent:
			names[e.ReplyID] = e.Name
		case event.RequireUserConfirmEvent:
			if len(e.ToolCalls) != 1 || e.ToolCalls[0].Name != "inspect_fixture" {
				r.Cancel()
				return fmt.Errorf("unexpected permission request")
			}
			fmt.Println("Host approves the example-owned fixture read.")
			if err := r.SubmitUserConfirm(e.GetEventID(), &event.UserConfirmResultEvent{ReplyID: e.ReplyID, ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: e.ToolCalls[0]}}}); err != nil {
				r.Cancel()
				return err
			}
		case event.TextBlockDeltaEvent:
			fmt.Printf("%s: %s\n", names[e.ReplyID], e.Delta)
		}
	}
	return r.Wait()
}
