package agent

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type validatedExternalTool struct {
	tool.BaseTool
	rejectInput, rejectResult bool
	resultChecks              int
}

func (*validatedExternalTool) IsExternalTool() bool { return true }
func (*validatedExternalTool) CheckPermissions(map[string]any, *permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.BehaviorAllow}
}
func (v *validatedExternalTool) ValidateInput(map[string]any) error {
	if v.rejectInput {
		return fmt.Errorf("invalid question")
	}
	return nil
}
func (v *validatedExternalTool) ValidateExternalResult(_ map[string]any, _ *message.ToolResultBlock) error {
	v.resultChecks++
	if v.rejectResult {
		return fmt.Errorf("invalid answers")
	}
	return nil
}

func TestExternalValidationAndMetadata(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		for _, scenario := range []struct {
			name                      string
			rejectInput, rejectResult bool
			hostError, denied         bool
		}{
			{name: "success"}, {name: "invalid_input", rejectInput: true},
			{name: "invalid_result", rejectResult: true},
			{name: "error_without_success_metadata", hostError: true, rejectResult: true},
			{name: "current_permission_denied", denied: true},
		} {
			t.Run(fmt.Sprintf("%s/resumed=%v", scenario.name, resumed), func(t *testing.T) {
				v := &validatedExternalTool{BaseTool: tool.BaseTool{ToolName: "question"}, rejectInput: scenario.rejectInput, rejectResult: scenario.rejectResult}
				tc := message.ToolCallBlock{Type: "tool_call", ID: "ask", Name: "question", Input: `{}`, State: message.ToolCallPending}
				final := model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true}
				mock := &mockChatModel{responses: []model.ChatResponse{{Content: []message.ContentBlock{tc}, IsLast: true}, final}}
				pctx := permission.NewContext(permission.ModeDefault)
				if scenario.denied {
					pctx.DenyRules["question"] = []permission.Rule{{ToolName: "question", Behavior: permission.BehaviorDeny}}
				}
				opts := []AgentOption{WithToolkit(tool.NewToolkit(v)), WithPermissionContext(pctx), WithReactConfig(ReactConfig{MaxIters: 3})}
				if resumed {
					tc.State = message.ToolCallSubmitted
					opts = append(opts, WithState(&AgentState{Context: []*message.Msg{{Name: "bot", Role: message.RoleAssistant, Content: []message.ContentBlock{tc}}}}))
					mock.responses = []model.ChatResponse{final}
				}
				a := NewUnifiedAgent("bot", "Help the user.", mock, opts...)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				stream, err := a.ReplyStream(ctx, "continue")
				if err != nil {
					t.Fatal(err)
				}
				metadata := map[string]any{"answers": []any{map[string]any{"question": "Proceed?", "selected": []string{"Yes"}}}}
				var handedOff, askedPermission bool
				var end *event.ToolResultEndEvent
				reconstructed := &message.Msg{}
				for ev := range stream {
					if start, ok := ev.(event.ReplyStartEvent); ok {
						reconstructed.ID = start.ReplyID
					}
					reconstructed.AppendEvent(ev)
					switch e := ev.(type) {
					case event.RequireExternalExecutionEvent:
						handedOff = true
						state, output, meta := message.ToolResultSuccess, "user answer", metadata
						if scenario.hostError {
							state, output, meta = message.ToolResultError, "host failed", nil
						}
						a.SubmitExternalResult(&event.ExternalExecutionResultEvent{ExecutionResults: []message.ToolResultBlock{{Type: "tool_result", ID: e.ToolCalls[0].ID, Name: "question", State: state, Output: output, Metadata: meta}}})
					case event.RequireUserConfirmEvent:
						askedPermission = true
						cancel()
					case event.ToolResultEndEvent:
						copyEnd := e
						end = &copyEnd
					}
				}
				wantHandoff := !scenario.rejectInput && !scenario.denied
				if handedOff != wantHandoff || askedPermission {
					t.Errorf("handoff=%v (want %v), redundant permission=%v", handedOff, wantHandoff, askedPermission)
				}
				wantState := message.ToolResultSuccess
				if scenario.rejectInput || scenario.rejectResult || scenario.hostError {
					wantState = message.ToolResultError
				}
				if scenario.denied {
					wantState = message.ToolResultDenied
				}
				if end == nil || end.State != wantState {
					t.Errorf("terminal result=%+v, want state %v", end, wantState)
				}
				wantChecks := 0
				if wantHandoff && !scenario.hostError {
					wantChecks = 1
				}
				if v.resultChecks != wantChecks {
					t.Errorf("success validation called %d times, want %d", v.resultChecks, wantChecks)
				}
				var recorded *message.ToolResultBlock
				for _, msg := range a.state.Context {
					for _, block := range msg.GetContentBlocks(message.ContentBlockToolResult) {
						tr := block.(message.ToolResultBlock)
						if tr.ID == "ask" {
							recorded = &tr
						}
					}
				}
				if recorded == nil || recorded.State != wantState {
					t.Fatalf("recorded result=%+v, want state %v", recorded, wantState)
				}
				if wantState == message.ToolResultSuccess {
					if !reflect.DeepEqual(recorded.Metadata, metadata) || end == nil || !reflect.DeepEqual(end.Metadata, metadata) {
						t.Errorf("successful metadata was not preserved: recorded=%#v end=%+v", recorded.Metadata, end)
					}
					blocks := reconstructed.GetContentBlocks(message.ContentBlockToolResult)
					if len(blocks) != 1 || !reflect.DeepEqual(blocks[0].(message.ToolResultBlock).Metadata, metadata) {
						t.Errorf("reconstructed metadata lost: %#v", blocks)
					}
				} else if len(recorded.Metadata) != 0 {
					t.Errorf("unvalidated success metadata retained on failure: %#v", recorded.Metadata)
				}
			})
		}
	}
}

func TestResumeExternalRequiresActiveExternalTool(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%v", missing), func(t *testing.T) {
			called := false
			tk := tool.NewToolkit()
			if !missing {
				tk = tool.NewToolkit(tool.NewFunctionTool("former_external", "Now local", nil,
					func(context.Context, map[string]any) (any, error) { called = true; return "must not execute", nil }))
			}
			tc := message.ToolCallBlock{Type: "tool_call", ID: "restored", Name: "former_external", Input: `{}`, State: message.ToolCallSubmitted}
			mock := &mockChatModel{responses: []model.ChatResponse{{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true}}}
			a := NewUnifiedAgent("bot", "Help.", mock, WithToolkit(tk), WithPermissionContext(permission.NewContext(permission.ModeDefault)), WithState(&AgentState{
				Context: []*message.Msg{{Name: "bot", Role: message.RoleAssistant, Content: []message.ContentBlock{tc}}},
			}))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			stream, err := a.ReplyStream(ctx, "continue")
			if err != nil {
				t.Fatal(err)
			}
			var ended bool
			for ev := range stream {
				switch e := ev.(type) {
				case event.RequireExternalExecutionEvent:
					t.Error("inactive external call was handed off")
					cancel()
				case event.RequireUserConfirmEvent:
					t.Error("inactive external call requested permission before being rejected")
					cancel()
				case event.ToolResultEndEvent:
					ended = true
					if e.State != message.ToolResultError {
						t.Errorf("state=%s, want error", e.State)
					}
				}
			}
			if called || !ended {
				t.Fatalf("local execution=%v, terminal error emitted=%v", called, ended)
			}
		})
	}
}

func TestExternalResultPreservesOrderedBlocks(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(fmt.Sprintf("resumed=%v", resumed), func(t *testing.T) {
			v := &validatedExternalTool{BaseTool: tool.BaseTool{ToolName: "question"}}
			tc := message.ToolCallBlock{Type: "tool_call", ID: "ordered", Name: "question", Input: `{}`, State: message.ToolCallPending}
			final := model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true}
			mock := &mockChatModel{responses: []model.ChatResponse{{Content: []message.ContentBlock{tc}, IsLast: true}, final}}
			opts := []AgentOption{WithToolkit(tool.NewToolkit(v)), WithPermissionContext(permission.NewContext(permission.ModeDefault))}
			if resumed {
				tc.State = message.ToolCallSubmitted
				opts = append(opts, WithState(&AgentState{Context: []*message.Msg{{Name: "bot", Role: message.RoleAssistant, Content: []message.ContentBlock{tc}}}}))
				mock.responses = []model.ChatResponse{final}
			}
			a := NewUnifiedAgent("bot", "Help.", mock, opts...)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			stream, err := a.ReplyStream(ctx, "continue")
			if err != nil {
				t.Fatal(err)
			}
			output := []message.ContentBlock{
				message.TextBlock{Type: "text", Text: "first"},
				message.DataBlock{Type: "data", ID: "image", Source: message.Base64Source{Type: "base64", MediaType: "image/png", Data: "aW1hZ2U="}},
				message.TextBlock{Type: "text", Text: "second"},
			}
			reconstructed := &message.Msg{}
			for ev := range stream {
				if start, ok := ev.(event.ReplyStartEvent); ok {
					reconstructed.ID = start.ReplyID
				}
				reconstructed.AppendEvent(ev)
				if _, ok := ev.(event.RequireExternalExecutionEvent); ok {
					a.SubmitExternalResult(&event.ExternalExecutionResultEvent{ExecutionResults: []message.ToolResultBlock{{Type: "tool_result", ID: tc.ID, Name: tc.Name, State: message.ToolResultSuccess, Output: output}}})
				}
			}
			for name, msgs := range map[string][]*message.Msg{"state": a.state.Context, "events": {reconstructed}} {
				found := false
				for _, msg := range msgs {
					for _, block := range msg.GetContentBlocks(message.ContentBlockToolResult) {
						tr := block.(message.ToolResultBlock)
						if tr.ID == tc.ID {
							found = true
							if !reflect.DeepEqual(tr.Output, output) {
								t.Errorf("%s output = %#v, want %#v", name, tr.Output, output)
							}
						}
					}
				}
				if !found {
					t.Errorf("%s has no tool result", name)
				}
			}
		})
	}
}
