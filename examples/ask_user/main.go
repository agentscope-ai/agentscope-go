// This offline example uses a scripted model and a simulated host answer to
// demonstrate AskUser's event protocol. It does not contact an LLM or collect
// a real user's approval. Replace the simulated answer with your application's
// UI before using the result to make a decision.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	fmt.Println("Offline AskUser demonstration: the model and host answer are simulated.")
	a := agent.NewUnifiedAgent("assistant", "Help choose a client language.", &scriptedModel{},
		agent.WithToolkit(tool.NewToolkit(tool.AskUserTool())),
		agent.WithPermissionContext(permission.NewContext(permission.ModeDefault)),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := a.ReplyStream(ctx, "Help me choose a language.")
	if err != nil {
		return err
	}
	for ev := range stream {
		switch e := ev.(type) {
		case event.RequireExternalExecutionEvent:
			for _, call := range e.ToolCalls {
				result, err := simulatedAnswer(&call)
				if err != nil {
					return err
				}
				a.SubmitExternalResult(&event.ExternalExecutionResultEvent{
					ReplyID: e.ReplyID, ExecutionResults: []message.ToolResultBlock{result},
				})
			}
		case event.RequireUserConfirmEvent:
			// This fixture never grants permission to execute other tools.
			var results []event.ConfirmResult
			for _, call := range e.ToolCalls {
				results = append(results, event.ConfirmResult{ToolCall: call, Confirmed: false})
			}
			a.SubmitUserConfirm(&event.UserConfirmResultEvent{ReplyID: e.ReplyID, ConfirmResults: results})
		case event.ToolResultEndEvent:
			if e.State != message.ToolResultSuccess {
				return fmt.Errorf("question failed: %s", e.State)
			}
			encoded, err := json.Marshal(e.Metadata)
			if err != nil {
				return err
			}
			fmt.Printf("Structured answers: %s\n", encoded)
		}
	}
	return ctx.Err()
}

func simulatedAnswer(call *message.ToolCallBlock) (message.ToolResultBlock, error) {
	var params tool.AskUserParams
	if err := json.Unmarshal([]byte(call.Input), &params); err != nil {
		return message.ToolResultBlock{}, err
	}
	var answers []tool.AskUserAnswer
	var output []string
	for _, q := range params.Questions {
		if len(q.Options) == 0 {
			return message.ToolResultBlock{}, fmt.Errorf("question has no options")
		}
		// A real host waits for the user's selection or free-text answer.
		selected := q.Options[0].Label
		fmt.Printf("%s Simulated answer: %s\n", q.Question, selected)
		answers = append(answers, tool.AskUserAnswer{Question: q.Question, Selected: []string{selected}})
		output = append(output, q.Question+" "+selected)
	}
	return message.ToolResultBlock{
		Type: "tool_result", ID: call.ID, Name: call.Name,
		State: message.ToolResultSuccess, Output: strings.Join(output, "\n"),
		Metadata: map[string]any{"answers": answers},
	}, nil
}

type scriptedModel struct{ asked bool }

func (m *scriptedModel) Chat(context.Context, []*message.Msg, ...model.CallOption) (*model.ChatResponse, error) {
	if m.asked {
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "Choice received."}}, IsLast: true}, nil
	}
	m.asked = true
	params := tool.AskUserParams{Questions: []tool.AskUserQuestion{{
		Question: "Which client language?", Header: "Language",
		Options: []tool.AskUserOption{{Label: "Go", Description: "Use the Go library"}, {Label: "Python", Description: "Use the Python library"}},
	}}}
	input, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{
		Type: "tool_call", ID: "choose-language", Name: "AskUser", Input: string(input), State: message.ToolCallPending,
	}}, IsLast: true}, nil
}

func (*scriptedModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (*scriptedModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 0 }
