package team

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

var teamCreateSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"name": {
			"type": "string",
			"description": "Display name for the team"
		},
		"description": {
			"type": "string",
			"description": "Team charter describing the team's purpose"
		}
	},
	"required": ["name", "description"]
}`)

var agentCreateSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"name": {
			"type": "string",
			"description": "Unique name for the new team member"
		},
		"description": {
			"type": "string",
			"description": "Description of the member's role"
		},
		"prompt": {
			"type": "string",
			"description": "Initial task or instructions for the member"
		}
	},
	"required": ["name", "description"]
}`)

var teamSaySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"content": {
			"type": "string",
			"description": "Message content to send"
		},
		"to": {
			"type": "string",
			"description": "Recipient name. Omit to broadcast to all members."
		}
	},
	"required": ["content"]
}`)

var teamDeleteSchema = json.RawMessage(`{
	"type": "object",
	"properties": {},
	"required": []
}`)

// teamToolBase provides shared state for team tools.
type teamToolBase struct {
	tool.BaseTool
	senderName string
}

// TeamCreateTool returns a tool that creates a new team with the caller as leader.
func TeamCreateTool(leaderName string) tool.Tool {
	return &teamCreateTool{
		teamToolBase: teamToolBase{
			BaseTool: tool.BaseTool{
				ToolName:        "team_create",
				ToolDescription: "Create a new team. You become the team leader. Team members can be added with agent_create.",
				ToolSchema:      teamCreateSchema,
			},
			senderName: leaderName,
		},
	}
}

type teamCreateTool struct {
	teamToolBase
}

func (t *teamCreateTool) Execute(ctx context.Context, args map[string]any) (*tool.ToolResponse, error) {
	if GetTeam(ctx) != nil {
		return tool.NewErrorResponse(fmt.Errorf("already in a team")), nil
	}

	name, _ := args["name"].(string)
	desc, _ := args["description"].(string)
	if name == "" {
		return tool.NewErrorResponse(fmt.Errorf("name is required")), nil
	}

	team := NewTeam(name, desc, t.senderName)
	return tool.NewTextResponse(fmt.Sprintf("Team %q created (id: %s). You are the leader.", name, team.ID)), nil
}

// AgentCreateTool returns a tool that creates a worker in the team.
func AgentCreateTool(leaderName string) tool.Tool {
	return &agentCreateTool{
		teamToolBase: teamToolBase{
			BaseTool: tool.BaseTool{
				ToolName:        "agent_create",
				ToolDescription: "Create a new team member. You must be the team leader.",
				ToolSchema:      agentCreateSchema,
			},
			senderName: leaderName,
		},
	}
}

type agentCreateTool struct {
	teamToolBase
}

func (t *agentCreateTool) Execute(ctx context.Context, args map[string]any) (*tool.ToolResponse, error) {
	team := GetTeam(ctx)
	if team == nil {
		return tool.NewErrorResponse(fmt.Errorf("not in a team; create one first with team_create")), nil
	}
	if team.LeaderName != t.senderName {
		return tool.NewErrorResponse(fmt.Errorf("only the team leader can create members")), nil
	}

	name, _ := args["name"].(string)
	desc, _ := args["description"].(string)
	if name == "" {
		return tool.NewErrorResponse(fmt.Errorf("name is required")), nil
	}

	member, err := team.AddMember(name, desc)
	if err != nil {
		return tool.NewErrorResponse(err), nil
	}

	if prompt, ok := args["prompt"].(string); ok && prompt != "" {
		_ = team.Send(t.senderName, name, prompt)
	}
	_ = member

	return tool.NewTextResponse(fmt.Sprintf("Member %q created in team %q.", name, team.Name)), nil
}

// TeamSayTool returns a tool for sending messages within the team.
func TeamSayTool(senderName string) tool.Tool {
	return &teamSayTool{
		teamToolBase: teamToolBase{
			BaseTool: tool.BaseTool{
				ToolName:        "team_say",
				ToolDescription: "Send a message to a team member by name, or broadcast to all members.",
				ToolSchema:      teamSaySchema,
			},
			senderName: senderName,
		},
	}
}

type teamSayTool struct {
	teamToolBase
}

func (t *teamSayTool) Execute(ctx context.Context, args map[string]any) (*tool.ToolResponse, error) {
	team := GetTeam(ctx)
	if team == nil {
		return tool.NewErrorResponse(fmt.Errorf("not in a team")), nil
	}

	content, _ := args["content"].(string)
	if content == "" {
		return tool.NewErrorResponse(fmt.Errorf("content is required")), nil
	}

	to, _ := args["to"].(string)
	if to == "" {
		if err := team.Broadcast(t.senderName, content); err != nil {
			return tool.NewErrorResponse(err), nil
		}
		return tool.NewTextResponse("Message broadcast to all team members."), nil
	}

	if err := team.Send(t.senderName, to, content); err != nil {
		if err == ErrLeaderRecipient {
			return tool.NewTextResponse("Message delivered to the leader."), nil
		}
		return tool.NewErrorResponse(err), nil
	}
	return tool.NewTextResponse(fmt.Sprintf("Message sent to %q.", to)), nil
}

// TeamDeleteTool returns a tool that disbands the team.
func TeamDeleteTool(leaderName string) tool.Tool {
	return &teamDeleteTool{
		teamToolBase: teamToolBase{
			BaseTool: tool.BaseTool{
				ToolName:        "team_delete",
				ToolDescription: "Disband the team. All members are removed.",
				ToolSchema:      teamDeleteSchema,
			},
			senderName: leaderName,
		},
	}
}

type teamDeleteTool struct {
	teamToolBase
}

func (t *teamDeleteTool) Execute(ctx context.Context, args map[string]any) (*tool.ToolResponse, error) {
	team := GetTeam(ctx)
	if team == nil {
		return tool.NewErrorResponse(fmt.Errorf("not in a team")), nil
	}
	if team.LeaderName != t.senderName {
		return tool.NewErrorResponse(fmt.Errorf("only the team leader can delete the team")), nil
	}

	team.Disband()
	return tool.NewTextResponse(fmt.Sprintf("Team %q has been disbanded.", team.Name)), nil
}

// NewLeaderToolkit creates a toolkit with all four team tools for a leader.
func NewLeaderToolkit(leaderName string) *tool.Toolkit {
	return tool.NewToolkit(
		TeamCreateTool(leaderName),
		AgentCreateTool(leaderName),
		TeamSayTool(leaderName),
		TeamDeleteTool(leaderName),
	)
}

// NewMemberToolkit creates a toolkit with the TeamSay tool for a worker.
func NewMemberToolkit(memberName string) *tool.Toolkit {
	return tool.NewToolkit(TeamSayTool(memberName))
}
