package participant

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// TestActivationCompactsAfterRestoredConfirmation covers #207 on a
// participant: its agent is restored with a call still asking for
// confirmation, as a host restores it after a restart, and the next
// activation brings new input. The activation re-asks, records the result
// after the input, runs one more round and compacts before the next model
// call. The compaction keeps the call's message, the input, the result and
// the round, and summarizes only the first message.
//
// Sizes, in rendered bytes (the script model counts a quarter token each):
// system 4, first message 2004, call message 428, input 9, result 219 and
// the round 251. The step after the round is at 728 tokens (trigger 700);
// the messages after the call's (with the system prompt) are 120 tokens and
// fit the 200-token reserve, and 227 with the call's message.
func TestActivationCompactsAfterRestoredConfirmation(t *testing.T) {
	var mu sync.Mutex
	main := 0
	restored := &agent.AgentState{SessionID: "cloud-session", Context: []*message.Msg{
		message.UserMsg("user", "q1 "+strings.Repeat("s", 2000)),
		message.AssistantMsg(cloudDesc.Name, []message.ContentBlock{
			message.TextBlock{Type: "text", Text: strings.Repeat("w", 400)},
			message.ToolCallBlock{Type: "tool_call", ID: "c0", Name: "web_fetch", Input: `{"url":"u"}`, State: message.ToolCallAsking},
		}),
	}}
	f := newFixture(t, fixtureOptions{
		mode:   permission.ModeDefault,
		noPrep: true,
		tools:  []tool.Tool{textTool("web_fetch", strings.Repeat("p", 200))},
		agent: []agent.AgentOption{
			agent.WithState(restored),
			agent.WithContextConfig(&agent.ContextConfig{ContextSize: 1000, TriggerRatio: 0.7, ReserveRatio: 0.2}),
		},
		respond: func(_ context.Context, _ int, o *model.CallOptions) (*model.ChatResponse, error) {
			for _, ts := range o.Tools {
				if ts.Function.Name == "generate_structured_output" {
					return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{
						Type: "tool_call", ID: "summary", Name: "generate_structured_output", State: message.ToolCallPending,
						Input: `{"task_overview":"T","current_state":"S","important_discoveries":"D","next_steps":"N","context_to_preserve":"P"}`,
					}}}, nil
				}
			}
			mu.Lock()
			defer mu.Unlock()
			main++
			if main == 1 {
				return resp("next", call("c1", "web_fetch", map[string]any{"url": "v"})), nil
			}
			return textResp("done"), nil
		},
	})

	asks := 0
	_, end := activate(t, f.p, foreground(1, "Q2-INPUT"), confirmAll(t, f.p, &asks))
	if end.Reason != types.ReplyCompleted || asks != 2 {
		t.Fatalf("end %+v after %d confirmations, want a finished reply after 2", end, asks)
	}
	if f.m.calls() != 3 {
		t.Fatalf("model calls = %d, want 3 (round, compression, final)", f.m.calls())
	}
	if compression := f.m.input(1); !strings.Contains(compression, "q1 ") || strings.Contains(compression, "Q2-INPUT") {
		t.Fatalf("the compression request should hold only the first message:\n%s", compression)
	}
	last := f.m.input(2)
	for _, want := range []string{`call web_fetch {"url":"u"}`, "Q2-INPUT", "result web_fetch: p", `call web_fetch {"url":"v"}`} {
		if !strings.Contains(last, want) {
			t.Errorf("the request after the compaction lacks %q:\n%s", want, last)
		}
	}
	if strings.Contains(last, "q1 ") {
		t.Errorf("the first message was not summarized:\n%s", last)
	}
}
