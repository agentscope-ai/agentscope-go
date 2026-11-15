package main

import (
	"context"
	"fmt"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/team"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// This example demonstrates the Agent Team system:
// - A leader agent creates a team and adds worker members
// - Workers receive messages in their inbox and can reply
// - The leader orchestrates collaboration via team_say tool
//
// The team tools (team_create, agent_create, team_say, team_delete) are
// provided to the leader agent. Workers get only team_say.

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	leaderName := "team-lead"

	// Create the team programmatically (instead of via tool call).
	t := team.NewTeam("research-team", "A team for collaborative research", leaderName)

	// Add two worker members.
	researcher, err := t.AddMember("researcher", "Performs in-depth research on topics")
	if err != nil {
		fmt.Println("Error adding researcher:", err)
		return
	}

	writer, err := t.AddMember("writer", "Writes summaries and reports")
	if err != nil {
		fmt.Println("Error adding writer:", err)
		return
	}

	fmt.Printf("Team %q created with leader %q\n", t.Name, t.LeaderName)
	fmt.Printf("Members: %v\n\n", t.Members())

	// Demonstrate message routing.
	fmt.Println("=== Message Routing ===")

	// Leader sends to researcher.
	if err := t.Send(leaderName, "researcher", "Research the latest trends in AI agents."); err != nil {
		fmt.Println("Send error:", err)
	}
	msg, err := researcher.Receive(context.Background())
	if err != nil {
		fmt.Println("Receive error:", err)
	} else {
		fmt.Printf("Researcher received: %s\n", *msg.GetTextContent("\n"))
	}

	// Leader broadcasts to all.
	if err := t.Broadcast(leaderName, "Meeting in 5 minutes."); err != nil {
		fmt.Println("Broadcast error:", err)
	}
	msg, _ = researcher.Receive(context.Background())
	fmt.Printf("Researcher received broadcast: %s\n", *msg.GetTextContent("\n"))
	msg, _ = writer.Receive(context.Background())
	fmt.Printf("Writer received broadcast: %s\n\n", *msg.GetTextContent("\n"))

	// Leader sends a message to leader → returns ErrLeaderRecipient.
	err = t.Send("researcher", leaderName, "Done with research!")
	if err == team.ErrLeaderRecipient {
		fmt.Println("Correctly caught: message to leader is handled separately")
	}

	// --- Demo with a leader agent using team tools ---
	fmt.Println("\n=== Leader Agent with Team Tools ===")

	// Build a toolkit with team tools plus a custom summarize tool.
	customTool := tool.NewFunctionTool("summarize", "Summarize a topic in one sentence", nil,
		func(ctx context.Context, input map[string]any) (any, error) {
			topic, _ := input["topic"].(string)
			return fmt.Sprintf("Summary of %q: AI agents are autonomous systems that use LLMs to reason and act.", topic), nil
		},
	)
	mergedToolkit := tool.NewToolkit(
		team.TeamCreateTool(leaderName),
		team.AgentCreateTool(leaderName),
		team.TeamSayTool(leaderName),
		team.TeamDeleteTool(leaderName),
		customTool,
	)

	// Create the leader agent with team context.
	ctx := team.WithTeam(context.Background(), t)
	leader := agent.NewUnifiedAgent(
		leaderName,
		"You are a team leader. You have team_say to communicate with team members. "+
			"You also have a summarize tool. When asked, use your tools.",
		cm,
		agent.WithToolkit(mergedToolkit),
		agent.WithReactConfig(agent.ReactConfig{MaxIters: 5}),
	)

	reply, err := leader.Reply(ctx, "Please summarize the topic of 'AI agents' for me.")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	if txt := reply.GetTextContent("\n"); txt != nil {
		fmt.Println("Leader:", *txt)
	}

	// Clean up.
	t.Disband()
	fmt.Println("\nTeam disbanded.")
}

func loadChatModelFromEnv() (model.ChatModel, error) {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return model.NewAnthropicChatModel(&model.AnthropicConfig{
			APIKey:          key,
			Model:           "claude-sonnet-4-20250514",
			MaxOutputTokens: 1024,
		})
	}
	if key := os.Getenv("DASHSCOPE_API_KEY"); key != "" {
		return model.NewDashScopeChatModel(model.DashScopeConfig{
			APIKey:  key,
			BaseURL: os.Getenv("DASHSCOPE_BASE_URL"),
			Model:   "qwen-plus",
		})
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		return model.NewOpenAIChatModel(model.OpenAIConfig{
			APIKey: key,
			Model:  "gpt-4o-mini",
		})
	}
	return nil, fmt.Errorf("set ANTHROPIC_API_KEY, DASHSCOPE_API_KEY, or OPENAI_API_KEY")
}
