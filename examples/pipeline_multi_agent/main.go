package main

import (
	"context"
	"fmt"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	asagent "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/pipeline"
)

// This example demonstrates using MsgHub + Pipeline to orchestrate a simple \"User -> Assistant\" conversation.

type EchoLLMAgent struct {
	asagent.AgentBase
	model model.ChatModel
}

func NewEchoLLMAgent(m model.ChatModel) *EchoLLMAgent {
	return &EchoLLMAgent{
		AgentBase: asagent.NewAgentBase(),
		model:     m,
	}
}

func (a *EchoLLMAgent) Reply(ctx context.Context, args ...any) (*message.Msg, error) {
	var last *message.Msg
	if len(args) > 0 {
		if m, ok := args[0].(*message.Msg); ok {
			last = m
		}
	}
	if last == nil {
		last = message.UserMsg("user", "Hello from pipeline.")
	}
	resp, err := a.model.Chat(ctx, []*message.Msg{last})
	if err != nil {
		return nil, err
	}
	reply := message.AssistantMsg("assistant", resp.Content)
	_ = a.Print(ctx, reply)
	return reply, nil
}

func (a *EchoLLMAgent) Observe(ctx context.Context, msgs []*message.Msg) error {
	_ = ctx
	_ = msgs
	return nil
}

func main() {
	as.Init()

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Println("please set OPENAI_API_KEY before running this example")
		return
	}

	cm, err := model.NewOpenAIChatModel(model.OpenAIConfig{
		APIKey: apiKey,
		Model:  "gpt-4o-mini",
	})
	if err != nil {
		panic(err)
	}

	assistant := NewEchoLLMAgent(cm)

	hub := pipeline.NewMsgHub()
	hub.Register("assistant", assistant)

	ctx := context.Background()
	pctx := &pipeline.Context{
		Ctx:      ctx,
		Agents:   map[string]asagent.Agent{"assistant": assistant},
		Messages: []*message.Msg{},
	}

	// Step1: construct user question.
	stepUser := func(c *pipeline.Context) error {
		msg := message.UserMsg("user", "Explain in one sentence what a pipeline is.")
		c.Messages = append(c.Messages, msg)
		return nil
	}

	// Step2: call assistant via MsgHub.
	stepAssistant := func(c *pipeline.Context) error {
		last := c.Messages[len(c.Messages)-1]
		reply, err := hub.RequestReply(c.Ctx, "user", "assistant", last)
		if err != nil {
			return err
		}
		c.Messages = append(c.Messages, reply)
		return nil
	}

	p := pipeline.New().
		Then(stepUser).
		Then(stepAssistant)

	if err := p.Run(pctx); err != nil {
		fmt.Println("pipeline error:", err)
		return
	}

	if len(pctx.Messages) > 0 {
		last := pctx.Messages[len(pctx.Messages)-1]
		if txt := last.GetTextContent("\n"); txt != nil {
			fmt.Println("pipeline final answer:", *txt)
		}
	}
}
