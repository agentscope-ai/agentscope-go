package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/a2a"
	asagent "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// This example demonstrates A2AAgent + HTTPClient: one process acts as both server and client.

// serverAgent is the Agent deployed on the HTTP server side.
type serverAgent struct {
	asagent.AgentBase
	model model.ChatModel
}

func newServerAgent(m model.ChatModel) *serverAgent {
	return &serverAgent{
		AgentBase: asagent.NewAgentBase(),
		model:     m,
	}
}

func (a *serverAgent) Reply(ctx context.Context, args ...any) (*message.Msg, error) {
	var msgs []*message.Msg
	for _, arg := range args {
		if m, ok := arg.(*message.Msg); ok {
			msgs = append(msgs, m)
		}
	}
	if len(msgs) == 0 {
		msgs = []*message.Msg{message.UserMsg("user", "Hello from A2A server")}
	}
	resp, err := a.model.Chat(ctx, msgs)
	if err != nil {
		return nil, err
	}
	reply := message.AssistantMsg("assistant", resp.Content)
	_ = a.Print(ctx, reply)
	return reply, nil
}

func (a *serverAgent) Observe(ctx context.Context, msgs []*message.Msg) error {
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

	// Start HTTP server-side Agent.
	srvAgent := newServerAgent(cm)

	http.HandleFunc("/a2a", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var payload struct {
			Messages []*message.Msg `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		ctx := r.Context()
		var last *message.Msg
		if len(payload.Messages) > 0 {
			last = payload.Messages[len(payload.Messages)-1]
		}
		reply, err := srvAgent.Reply(ctx, last)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		resp := struct {
			Reply *message.Msg `json:"reply"`
		}{Reply: reply}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	go func() {
		log.Println("A2A HTTP server listening on :8080")
		_ = http.ListenAndServe(":8080", nil)
	}()

	// Client: use A2AAgent to call the HTTP server-side agent.
	client := &a2a.HTTPClient{Endpoint: "http://localhost:8080/a2a"}
	a2aAgent := asagent.NewA2AAgent("remote_assistant", client)

	ctx := context.Background()
	msg := message.UserMsg("user", "Introduce yourself in one sentence (via A2A HTTP call).")
	reply, err := a2aAgent.Reply(ctx, msg)
	if err != nil {
		fmt.Println("A2AAgent error:", err)
		return
	}
	if txt := reply.GetTextContent("\n"); txt != nil {
		fmt.Println("A2A reply:", *txt)
	}
}
