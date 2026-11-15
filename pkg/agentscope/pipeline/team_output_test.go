package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

type teamEventFixture struct {
	middleware.BaseMiddleware
	events func() []event.Event
}

func (m *teamEventFixture) OnReply(ctx context.Context, _ middleware.ReplyInput, _ middleware.ReplyHandler) <-chan event.Event {
	out := make(chan event.Event, 32)
	go func() {
		defer close(out)
		for _, ev := range m.events() {
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func TestTeamMemberTerminalAndFinalTextPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		reason                             types.ReplyFinishedReason
		partial, missing, mediaOnly, mixed bool
		want                               message.ToolResultState
		terminalError, wantOutput          string
	}{
		{name: "normal", reason: types.ReplyCompleted, want: message.ToolResultSuccess},
		{name: "max iterations", reason: types.ReplyExceedMaxIters, want: message.ToolResultError, wantOutput: "pipeline: A finished as exceed_max_iters"},
		{name: "member error", reason: types.ReplyError, want: message.ToolResultError, terminalError: "fixture terminal failure", wantOutput: "pipeline: A finished as error: fixture terminal failure"},
		{name: "member interrupted", reason: types.ReplyInterrupted, want: message.ToolResultInterrupted, wantOutput: "pipeline: A finished as interrupted"},
		{name: "missing terminal", missing: true, want: message.ToolResultError},
		{name: "partial final model", reason: types.ReplyCompleted, partial: true, want: message.ToolResultError, wantOutput: "pipeline: A final model round is incomplete"},
		{name: "partial model with terminal error", reason: types.ReplyError, partial: true, want: message.ToolResultError, terminalError: "fixture terminal failure", wantOutput: "pipeline: A finished as error: fixture terminal failure"},
		{name: "media only", reason: types.ReplyCompleted, mediaOnly: true, want: message.ToolResultError},
		{name: "mixed media text", reason: types.ReplyCompleted, mixed: true, want: message.ToolResultSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &teamEventFixture{events: func() []event.Event {
				ev := []event.Event{event.NewReplyStartEvent("session", "old", "A", message.RoleAssistant), event.NewModelCallStartEvent("old", "fixture"), event.NewTextBlockDeltaEvent("old", "draft", "OLD_REPLY_NARRATIVE"),
					event.NewReplyStartEvent("session", "reply", "A", message.RoleAssistant), event.NewModelCallStartEvent("reply", "fixture"), event.NewModelCallEndEvent("reply", 1, 1), event.NewTextBlockStartEvent("reply", "early"), event.NewTextBlockDeltaEvent("reply", "early", "EARLY_MODEL_NARRATIVE"),
					event.NewModelCallStartEvent("reply", "fixture")}
				if !tc.mediaOnly {
					ev = append(ev, event.NewTextBlockStartEvent("reply", "final"), event.NewTextBlockDeltaEvent("reply", "final", "final answer"))
				}
				if tc.mediaOnly || tc.mixed {
					ev = append(ev, event.NewDataBlockStartEvent("reply", "data", "image/png"), event.NewDataBlockDeltaEvent("reply", "data", "YQ==", "image/png"))
				}
				ev = append(ev, event.NewModelCallEndEvent("reply", 1, 1), event.NewModelCallEndEvent("reply", 3, 4)) // compression usage has no matching start
				if tc.partial {
					ev = append(ev, event.NewCustomEvent("reply", "model_partial_response", nil))
				}
				if !tc.missing {
					terminal := event.NewReplyEndEventWithReason("session", "reply", tc.reason)
					if tc.terminalError != "" {
						terminal.Error = &types.ReplyErrorInfo{Message: tc.terminalError}
					}
					ev = append(ev, terminal)
				}
				return ev
			}}
			member := teamMember("A", nil, agent.WithMiddlewares(fixture))
			p, _ := NewTeamPipeline(teamLeader([]message.ToolCallBlock{teamCall("assignment", TeamAssignToolName, `{"member":"A","prompt":"task"}`)}, func(results []message.ToolResultBlock) error {
				if len(results) != 1 || results[0].State != tc.want {
					return fmt.Errorf("unexpected member state %v", results)
				}
				if tc.want == message.ToolResultSuccess && results[0].GetOutputText() != "final answer" {
					return fmt.Errorf("wrong final text: %q", results[0].GetOutputText())
				}
				if tc.wantOutput != "" && results[0].GetOutputText() != tc.wantOutput {
					return fmt.Errorf("wrong member diagnostic: got %q, want %q", results[0].GetOutputText(), tc.wantOutput)
				}
				return nil
			}), member)
			if _, err := teamDrain(t, p, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type teamStreamFixture struct{ call int }

func (*teamStreamFixture) Chat(context.Context, []*message.Msg, ...model.CallOption) (*model.ChatResponse, error) {
	return nil, fmt.Errorf("unexpected buffered call")
}
func (*teamStreamFixture) CountTokens([]*message.Msg, []model.ToolSchema) int { return 0 }
func (m *teamStreamFixture) ChatStream(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	m.call++
	ch := make(chan model.ChatResponse, 2)
	if m.call == 1 {
		ch <- model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "PRIVATE_MEMBER_DRAFT"}}}
		ch <- model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "PRIVATE_MEMBER_DRAFT"}, teamCall("inspect", "inspect", `{}`)}}
	} else {
		ch <- model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "streamed final"}}}
		ch <- model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "streamed final"}}}
	}
	close(ch)
	return ch, nil
}

func TestTeamActualStreamingUsesOnlyFinalModelRound(t *testing.T) {
	member := TeamMember{Name: "A", Factory: func(context.Context) (*agent.UnifiedAgent, error) {
		inspect := tool.NewFunctionTool("inspect", "fixture", nil, func(context.Context, map[string]any) (any, error) { return "PRIVATE_TOOL_DATA", nil })
		return agent.NewUnifiedAgent("A", "fixture", &teamStreamFixture{}, append(teamOptions(inspect), agent.WithModelStreaming(true))...), nil
	}}
	p, _ := NewTeamPipeline(teamLeader([]message.ToolCallBlock{teamCall("assignment", TeamAssignToolName, `{"member":"A","prompt":"task"}`)}, func(results []message.ToolResultBlock) error {
		if len(results) != 1 || results[0].State != message.ToolResultSuccess || results[0].GetOutputText() != "streamed final" {
			return fmt.Errorf("wrong streamed answer %v", results)
		}
		return nil
	}), member)
	if _, err := teamDrain(t, p, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTeamActualMemberFailuresAreNonSuccess(t *testing.T) {
	for _, kind := range []string{"model error", "max iterations", "partial final round", "media only", "mixed text media"} {
		t.Run(kind, func(t *testing.T) {
			n := 0
			member := teamMember("A", func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
				n++
				switch kind {
				case "model error":
					return nil, fmt.Errorf("fixture permanent error")
				case "partial final round":
					return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "truncated answer"}}, Error: errors.New("fixture truncated final round")}, nil
				case "max iterations":
					if n == 1 {
						return &model.ChatResponse{Content: []message.ContentBlock{teamCall("noop", "noop", `{}`)}}, nil
					}
					return teamText("exhausted partial summary"), nil
				default:
					blocks := []message.ContentBlock{message.DataBlock{Type: "data", Source: message.URLSource{Type: "url", URL: "https://example.invalid/image.png", MediaType: "image/png"}}}
					if kind == "mixed text media" {
						blocks = append(blocks, message.TextBlock{Type: "text", Text: "text part"})
					}
					return &model.ChatResponse{Content: blocks}, nil
				}
			}, agent.WithReactConfig(agent.ReactConfig{MaxIters: 1}), agent.WithModelConfig(agent.ModelConfig{MaxRetries: 1}), agent.WithToolkit(tool.NewToolkit(tool.NewFunctionTool("noop", "fixture", nil, func(context.Context, map[string]any) (any, error) { return "done", nil }))))
			p, _ := NewTeamPipeline(teamLeader([]message.ToolCallBlock{teamCall("assignment", TeamAssignToolName, `{"member":"A","prompt":"task"}`)}, func(results []message.ToolResultBlock) error {
				if len(results) != 1 {
					return fmt.Errorf("missing result")
				}
				if kind == "mixed text media" {
					if results[0].State != message.ToolResultSuccess || results[0].GetOutputText() != "text part" {
						return fmt.Errorf("wrong text subset %v", results)
					}
				} else if results[0].State != message.ToolResultError {
					return fmt.Errorf("failure reported as success: %v", results)
				}
				if kind == "partial final round" && results[0].GetOutputText() != "pipeline: A final model round is incomplete" {
					return fmt.Errorf("misleading partial-output diagnostic: %q", results[0].GetOutputText())
				}
				return nil
			}), member)
			if _, err := teamDrain(t, p, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTeamUnsupportedAndOwnedNestedInput(t *testing.T) {
	cycle := map[string]any{}
	cycle["self"] = cycle
	for _, value := range []any{cycle, make(chan int), func() {}, map[int]string{1: "x"}, struct{ private int }{1}} {
		if _, err := cloneTeamInput(value); !errors.Is(err, ErrTeamInput) {
			t.Fatalf("unsupported %T accepted", value)
		}
	}
	meta := map[string]any{"blocks": []message.ContentBlock{message.DataBlock{Type: "data", Source: message.Base64Source{Type: "base64", Data: "YQ==", MediaType: "text/plain"}}}, "bytes": []byte{1, 2}, "array": [2]int{3, 4}, "pointer": &[]string{"owned"}, "nil": (*string)(nil)}
	cp, err := cloneTeamInput(meta)
	if err != nil {
		t.Fatal(err)
	}
	meta["bytes"].([]byte)[0] = 9
	(*meta["pointer"].(*[]string))[0] = "changed"
	if cp["bytes"].([]byte)[0] != 1 || (*cp["pointer"].(*[]string))[0] != "owned" {
		t.Fatal("nested mutable data was shared")
	}
	if _, ok := cp["blocks"].([]message.ContentBlock)[0].(message.DataBlock).Source.(message.Base64Source); !ok {
		t.Fatal("content source type lost")
	}
	if _, err := cloneTeamInput(strings.Repeat("x", 20)); err != nil {
		t.Fatal(err)
	}
}

func TestTeamNormalLeaderDoesNotRequireMemberTransferText(t *testing.T) {
	for _, kind := range []string{"empty", "base64 media", "url media"} {
		t.Run(kind, func(t *testing.T) {
			leader := func(_ context.Context, assign tool.Tool) (*agent.UnifiedAgent, error) {
				m := &goalFixtureModel{call: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
					resp := &model.ChatResponse{}
					if kind == "base64 media" {
						resp.Content = []message.ContentBlock{message.DataBlock{Type: "data", Source: message.Base64Source{Type: "base64", Data: "YQ==", MediaType: "image/png"}}}
					}
					if kind == "url media" {
						resp.Content = []message.ContentBlock{message.DataBlock{Type: "data", Source: message.URLSource{Type: "url", URL: "https://example.invalid/image.png", MediaType: "image/png"}}}
					}
					return resp, nil
				}}
				return agent.NewUnifiedAgent("leader", "fixture", m, teamOptions(assign)...), nil
			}
			p, _ := NewTeamPipeline(leader, teamMember("A", nil))
			events, err := teamDrain(t, p, nil)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "base64 media" {
				found := false
				for _, ev := range events {
					if _, ok := ev.(event.DataBlockStartEvent); ok {
						found = true
					}
				}
				if !found {
					t.Fatal("leader data event was lost")
				}
			}
		})
	}
}
