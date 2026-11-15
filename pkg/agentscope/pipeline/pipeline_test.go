package pipeline

import (
	"context"
	"testing"

	asagent "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestPipelineThenAndIf(t *testing.T) {
	var called []string

	p := New()
	p.Then(func(c *Context) error {
		called = append(called, "step1")
		return nil
	}).If(
		func(c *Context) bool { return true },
		func(c *Context) error {
			called = append(called, "step2")
			return nil
		},
	)

	ctx := &Context{Ctx: context.Background()}
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(called) != 2 || called[0] != "step1" || called[1] != "step2" {
		t.Fatalf("unexpected call order: %#v", called)
	}
}

type fakeAgent struct {
	asagent.AgentBase
	id       string
	lastSeen *message.Msg
	reply    *message.Msg
}

func (f *fakeAgent) ID() string { return f.id }

func (f *fakeAgent) Reply(ctx context.Context, args ...any) (*message.Msg, error) {
	return f.reply, nil
}

func (f *fakeAgent) Observe(ctx context.Context, msgs []*message.Msg) error {
	if len(msgs) > 0 {
		f.lastSeen = msgs[len(msgs)-1]
	}
	return nil
}

func TestMsgHubBroadcastAndRequestReply(t *testing.T) {
	h := NewMsgHub()
	assistant := &fakeAgent{
		id:    "assistant",
		reply: message.AssistantMsg("assistant", "ok"),
	}
	h.Register("assistant", assistant)

	ctx := context.Background()
	msg := message.UserMsg("user", "hi")

	if err := h.Broadcast(ctx, msg); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if assistant.lastSeen == nil || assistant.lastSeen.ID != msg.ID {
		t.Fatalf("assistant did not observe message")
	}

	reply, err := h.RequestReply(ctx, "user", "assistant", msg)
	if err != nil {
		t.Fatalf("RequestReply: %v", err)
	}
	if reply == nil {
		t.Fatalf("nil reply")
	}
}
