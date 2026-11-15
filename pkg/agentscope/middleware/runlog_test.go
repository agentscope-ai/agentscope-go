package middleware

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestRunJSONL_WritesEventsAndModelCalls(t *testing.T) {
	var buf bytes.Buffer
	rl := NewRunJSONL(&buf)

	core := func(_ context.Context, _ ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, 2)
		ch <- event.NewReplyStartEvent("s", "r1", "a", message.RoleAssistant)
		ch <- event.NewReplyEndEvent("s", "r1")
		close(ch)
		return ch
	}

	ctx := context.Background()
	out := rl.OnReply(ctx, ReplyInput{AgentName: "a"}, core)
	for range out {
	}
	// Also record a model call line.
	_, _ = rl.OnModelCall(ctx, &ModelCallInput{AgentName: "a", ModelName: "m1",
		Messages: []*message.Msg{message.UserMsg("u", "SECRET-input")}},
		func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
			return &model.ChatResponse{Usage: &model.ChatUsage{InputTokens: 3}}, nil
		})

	logged := buf.String()
	if !strings.Contains(logged, `"reply_input"`) || !strings.Contains(logged, `"model_call"`) {
		t.Errorf("missing line types:\n%s", logged)
	}
	if strings.Count(logged, `"event"`) < 2 {
		t.Error("event lines missing")
	}
	if strings.Contains(logged, "SECRET-input") {
		t.Error("raw secret leaked into run log without redactor")
	}

}

func TestRunJSONL_RedactorApplied(t *testing.T) {
	var buf bytes.Buffer
	rl := NewRunJSONL(&buf)
	WithRunLogRedactor(func(s string) string { return strings.ReplaceAll(s, "SECRET", "***") })(rl)

	ctx := context.Background()
	_, _ = rl.OnModelCall(ctx, &ModelCallInput{AgentName: "a",
		Messages: []*message.Msg{message.UserMsg("u", "SECRET")}},
		func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
			return &model.ChatResponse{}, nil
		})
	if strings.Contains(buf.String(), "SECRET") {
		t.Error("redactor not applied")
	}
}
