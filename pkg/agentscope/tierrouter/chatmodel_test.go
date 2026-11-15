package tierrouter

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestChatRouterPreferredAndReplay(t *testing.T) {
	cloud := NewFakeChatModel(TierCloud)
	cloud.Reply = "cloud answer"
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierCloud}}, func() int64 { return 1 }, cloud)
	op := &Operation{ID: "chat-preferred", TaskEpoch: 1, Kind: "chat", Content: "policy-content"}
	ctx := WithChatOperation(context.Background(), op)

	resp, err := r.Chat(ctx, chatMessages("hello"), model.WithTemperature(0.25))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.GetTextContent() != "cloud answer" || len(cloud.Requests()) != 1 {
		t.Fatalf("response=%q requests=%d", resp.GetTextContent(), len(cloud.Requests()))
	}
	resp.Content[0] = message.TextBlock{Type: "text", Text: "mutated"}
	resp.Metadata["nested"].([]any)[0].(map[string]any)["value"] = "mutated"

	replayed, err := r.Chat(WithChatOperation(context.Background(), &Operation{ID: op.ID, TaskEpoch: 1, Kind: "chat", Content: op.Content, SubmittedAt: time.Now().Add(time.Hour)}), chatMessages("hello"), model.WithTemperature(0.25))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed.GetTextContent() != "cloud answer" || len(cloud.Requests()) != 1 {
		t.Fatalf("replay response=%q requests=%d", replayed.GetTextContent(), len(cloud.Requests()))
	}
	if got := replayed.Metadata["nested"].([]any)[0].(map[string]any)["value"]; got != "original" {
		t.Fatalf("cached metadata mutated: %v", got)
	}
	rec, ok := r.ChatSnapshot(op.ID)
	if !ok || rec.State != StateCompleted || len(rec.Attempts) != 1 || rec.Attempts[0].Tier != TierCloud {
		t.Fatalf("record: %+v", rec)
	}
}

func TestChatRouterFallbackFreezesOptions(t *testing.T) {
	cloud := NewFakeChatModel(TierCloud)
	cloud.FailNext = 1
	edge := NewFakeChatModel(TierEdge)
	edge.Reply = "edge answer"
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierCloud, TierEdge}}, func() int64 { return 1 }, cloud, edge)

	var calls atomic.Int32
	option := func(o *model.CallOptions) {
		value := float64(calls.Add(1))
		o.Temperature = &value
	}
	resp, err := r.Chat(WithChatOperation(context.Background(), &Operation{ID: "chat-fallback", TaskEpoch: 1}), chatMessages("hello"), option)
	if err != nil || resp.GetTextContent() != "edge answer" {
		t.Fatalf("response=%v err=%v", resp, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("option applied %d times", calls.Load())
	}
	cloudReq := cloud.Requests()[0]
	edgeReq := edge.Requests()[0]
	if cloudReq.Options.Temperature == nil || edgeReq.Options.Temperature == nil ||
		*cloudReq.Options.Temperature != 1 || *edgeReq.Options.Temperature != 1 {
		t.Fatalf("frozen options: cloud=%+v edge=%+v", cloudReq.Options, edgeReq.Options)
	}
	rec, _ := r.ChatSnapshot("chat-fallback")
	if len(rec.Attempts) != 2 || rec.Attempts[0].Outcome != AttemptFailed || rec.Attempts[1].Outcome != AttemptCompleted {
		t.Fatalf("attempts: %+v", rec.Attempts)
	}
}

func TestChatRouterRequestIdentity(t *testing.T) {
	device := NewFakeChatModel(TierDevice)
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierDevice}}, func() int64 { return 3 }, device)
	base := &Operation{ID: "chat-identity", TaskEpoch: 3, Kind: "chat", Content: "one", TTL: time.Minute}
	if _, err := r.Chat(WithChatOperation(context.Background(), base), chatMessages("hello")); err != nil {
		t.Fatalf("first chat: %v", err)
	}
	for name, mutate := range map[string]func(*Operation){
		"content": func(op *Operation) { op.Content = "two" },
		"epoch":   func(op *Operation) { op.TaskEpoch = 4 },
		"ttl":     func(op *Operation) { op.TTL = 2 * time.Minute },
	} {
		t.Run(name, func(t *testing.T) {
			op := *base
			mutate(&op)
			if _, err := r.Chat(WithChatOperation(context.Background(), &op), chatMessages("hello")); !errors.Is(err, ErrIDConflict) {
				t.Fatalf("want ID conflict, got %v", err)
			}
		})
	}
	if _, err := r.Chat(WithChatOperation(context.Background(), base), chatMessages("different")); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("message conflict: %v", err)
	}
	if _, err := r.Chat(WithChatOperation(context.Background(), base), chatMessages("hello"), model.WithMaxTokens(10)); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("option conflict: %v", err)
	}
	if len(device.Requests()) != 1 {
		t.Fatalf("conflicting calls dispatched: %d", len(device.Requests()))
	}
}

func TestChatRouterConfigurationAndCapabilities(t *testing.T) {
	device := NewFakeChatModel(TierDevice)
	device.ContextWindow = 4096
	device.TokenEstimate = 10
	edge := NewFakeChatModel(TierEdge)
	edge.ContextWindow = 2048
	edge.TokenEstimate = 20
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierDevice, TierEdge}}, func() int64 { return 1 }, device, edge)
	if got := r.ContextSize(); got != 2048 {
		t.Fatalf("context size: %d", got)
	}
	if got := r.CountTokens(chatMessages("hello"), nil); got != 20 {
		t.Fatalf("count tokens: %d", got)
	}
	if _, err := r.Chat(context.Background(), chatMessages("hello")); !errors.Is(err, ErrChatOperationRequired) {
		t.Fatalf("missing operation: %v", err)
	}
	ctx := WithChatOperation(context.Background(), &Operation{ID: "unsupported", TaskEpoch: 1})
	if _, err := r.Chat(ctx, chatMessages("hello"), model.WithTools([]model.ToolSchema{{Type: "function"}})); !errors.Is(err, ErrChatUnsupported) {
		t.Fatalf("tools: %v", err)
	}
	if _, err := r.ChatStream(ctx, chatMessages("hello")); !errors.Is(err, model.ErrStreamNotSupported) {
		t.Fatalf("stream: %v", err)
	}
	if len(device.Requests())+len(edge.Requests()) != 0 {
		t.Fatal("unsupported calls dispatched")
	}
}

func chatMessages(text string) []*message.Msg {
	return []*message.Msg{{
		ID:        "message-" + text,
		Name:      "user",
		Role:      message.RoleUser,
		Content:   []message.ContentBlock{message.TextBlock{Type: "text", Text: text}},
		Timestamp: "2026-01-01 00:00:00.000",
	}}
}

func newChatHarness(t *testing.T, policy Policy, epoch func() int64, deployments ...*FakeChatModel) *ChatRouter {
	t.Helper()
	registered := make([]ChatDeployment, 0, len(deployments))
	for _, deployment := range deployments {
		registered = append(registered, ChatDeployment{Tier: deployment.Tier(), Model: deployment})
	}
	r, err := NewChatRouter(ChatRouterConfig{Policy: policy, Epoch: epoch, Deployments: registered, ContextFallback: 1024})
	if err != nil {
		t.Fatalf("new chat router: %v", err)
	}
	return r
}
