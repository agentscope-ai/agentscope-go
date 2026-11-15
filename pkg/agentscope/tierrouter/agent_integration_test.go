package tierrouter_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tierrouter"
)

func TestUnifiedAgentReplyUsesChatRouter(t *testing.T) {
	cloud := tierrouter.NewFakeChatModel(tierrouter.TierCloud)
	cloud.FailNext = 1
	edge := tierrouter.NewFakeChatModel(tierrouter.TierEdge)
	edge.Reply = "edge answer"
	router := newAgentRouter(t, func() int64 { return 1 }, cloud, edge)
	fallback := tierrouter.NewFakeChatModel(tierrouter.TierDevice)
	state := &agent.AgentState{SessionID: "chat-router-success"}
	replyAgent := agent.NewUnifiedAgent("bot", "Answer briefly.", router,
		agent.WithModelStreaming(false),
		agent.WithModelConfig(agent.ModelConfig{MaxRetries: 1, FallbackModel: fallback}),
		agent.WithState(state))
	ctx := tierrouter.WithChatOperation(context.Background(), &tierrouter.Operation{ID: "agent-success", TaskEpoch: 1, Kind: "chat"})
	reply, err := replyAgent.Reply(ctx, "hello")
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if text := reply.GetTextContent("\n"); text == nil || *text != "edge answer" {
		t.Fatalf("reply text: %v", text)
	}
	if len(cloud.Requests()) != 1 || len(edge.Requests()) != 1 || len(fallback.Requests()) != 0 {
		t.Fatalf("cloud=%d edge=%d agent-fallback=%d", len(cloud.Requests()), len(edge.Requests()), len(fallback.Requests()))
	}
	record, ok := router.ChatSnapshot("agent-success")
	if !ok || record.State != tierrouter.StateCompleted || len(record.Attempts) != 2 {
		t.Fatalf("record: %+v", record)
	}
	assistant := lastAssistant(state.Context)
	if assistant == nil || assistant.Usage == nil || assistant.Usage.InputTokens != 1 || assistant.Usage.OutputTokens != 1 {
		t.Fatalf("assistant history: %+v", assistant)
	}
}

func TestUnifiedAgentRejectsStaleResponseAndRecovers(t *testing.T) {
	epoch := int64(1)
	cloud := tierrouter.NewFakeChatModel(tierrouter.TierCloud)
	cloud.Reply = "old answer"
	cloud.BeforeReturn = func() { epoch = 2 }
	router := newAgentRouter(t, func() int64 { return epoch }, cloud)
	state := &agent.AgentState{SessionID: "chat-router-stale"}
	replyAgent := agent.NewUnifiedAgent("bot", "Answer briefly.", router,
		agent.WithModelStreaming(false),
		agent.WithModelConfig(agent.ModelConfig{MaxRetries: 1}),
		agent.WithState(state))

	ctx := tierrouter.WithChatOperation(context.Background(), &tierrouter.Operation{ID: "agent-stale", TaskEpoch: 1})
	reply, err := replyAgent.Reply(ctx, "first")
	if err == nil || reply != nil {
		t.Fatalf("stale reply=%v err=%v", reply, err)
	}
	if len(state.Context) != 1 || state.Context[0].Role != message.RoleUser {
		t.Fatalf("stale response entered history: %+v", state.Context)
	}

	cloud.BeforeReturn = nil
	cloud.Reply = "current answer"
	ctx = tierrouter.WithChatOperation(context.Background(), &tierrouter.Operation{ID: "agent-current", TaskEpoch: 2})
	reply, err = replyAgent.Reply(ctx, "second")
	if err != nil {
		t.Fatalf("current reply: %v", err)
	}
	if text := reply.GetTextContent("\n"); text == nil || *text != "current answer" {
		t.Fatalf("current text: %v", text)
	}
	for _, msg := range state.Context {
		if text := msg.GetTextContent("\n"); text != nil && *text == "old answer" {
			t.Fatal("old answer entered subsequent history")
		}
	}
}

func TestUnifiedAgentDoesNotRetryRouterFailure(t *testing.T) {
	cloud := tierrouter.NewFakeChatModel(tierrouter.TierCloud)
	cloud.FailNext = 1
	router := newAgentRouter(t, func() int64 { return 1 }, cloud)
	state := &agent.AgentState{SessionID: "chat-router-failure"}
	replyAgent := agent.NewUnifiedAgent("bot", "Answer briefly.", router,
		agent.WithModelStreaming(false),
		agent.WithModelConfig(agent.ModelConfig{MaxRetries: 1}),
		agent.WithState(state))
	ctx := tierrouter.WithChatOperation(context.Background(), &tierrouter.Operation{ID: "agent-failure", TaskEpoch: 1})
	if reply, err := replyAgent.Reply(ctx, "hello"); err == nil || reply != nil {
		t.Fatalf("reply=%v err=%v", reply, err)
	}
	if len(cloud.Requests()) != 1 {
		t.Fatalf("cloud requests=%d", len(cloud.Requests()))
	}
	record, _ := router.ChatSnapshot("agent-failure")
	if record.State != tierrouter.StateExhausted || len(record.Attempts) != 3 {
		t.Fatalf("record: %+v", record)
	}
}

func TestUnifiedAgentRejectsInvalidResponse(t *testing.T) {
	cloud := tierrouter.NewFakeChatModel(tierrouter.TierCloud)
	cloud.ReturnResponse = true
	cloud.Response = &model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "partial"}},
		IsLast:  true,
		Error:   errors.New("in-band failure"),
	}
	edge := tierrouter.NewFakeChatModel(tierrouter.TierEdge)
	router := newAgentRouter(t, func() int64 { return 1 }, cloud, edge)
	state := &agent.AgentState{SessionID: "chat-router-invalid"}
	replyAgent := agent.NewUnifiedAgent("bot", "Answer briefly.", router,
		agent.WithModelStreaming(false),
		agent.WithModelConfig(agent.ModelConfig{MaxRetries: 1}),
		agent.WithState(state))
	ctx := tierrouter.WithChatOperation(context.Background(), &tierrouter.Operation{ID: "agent-invalid", TaskEpoch: 1})
	if reply, err := replyAgent.Reply(ctx, "hello"); err == nil || reply != nil {
		t.Fatalf("reply=%v err=%v", reply, err)
	}
	if len(edge.Requests()) != 0 {
		t.Fatal("invalid response triggered fallback")
	}
	if len(state.Context) != 1 || state.Context[0].Role != message.RoleUser {
		t.Fatalf("invalid response entered history: %+v", state.Context)
	}
}

func newAgentRouter(t *testing.T, epoch func() int64, models ...*tierrouter.FakeChatModel) *tierrouter.ChatRouter {
	t.Helper()
	deployments := make([]tierrouter.ChatDeployment, 0, len(models))
	for _, chatModel := range models {
		deployments = append(deployments, tierrouter.ChatDeployment{Tier: chatModel.Tier(), Model: chatModel})
	}
	router, err := tierrouter.NewChatRouter(tierrouter.ChatRouterConfig{
		Policy:          tierrouter.StaticPolicy{Default: []tierrouter.Tier{tierrouter.TierCloud, tierrouter.TierEdge, tierrouter.TierDevice}},
		Epoch:           epoch,
		Deployments:     deployments,
		ContextFallback: 1024,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return router
}

func lastAssistant(msgs []*message.Msg) *message.Msg {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.RoleAssistant {
			return msgs[i]
		}
	}
	return nil
}
