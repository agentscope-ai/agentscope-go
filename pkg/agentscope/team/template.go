package team

import (
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

// SubAgentTemplate defines a reusable blueprint for creating worker agents.
type SubAgentTemplate struct {
	Type                 string // routing key (e.g. "researcher", "coder")
	Description          string // agent-readable description
	SystemPromptTemplate string // format string with placeholders

	ContextConfig *agent.ContextConfig // optional context compression settings
	ReactConfig   *agent.ReactConfig   // optional ReAct loop settings
	PermissionCtx *permission.Context  // optional permission context
}

// TemplateVars holds the values for substituting template placeholders.
type TemplateVars struct {
	TeamName          string
	TeamDescription   string
	MemberName        string
	MemberDescription string
	LeaderName        string
}

// Render substitutes placeholders in the template with the given values.
func (t *SubAgentTemplate) Render(vars *TemplateVars) string {
	r := strings.NewReplacer(
		"{team_name}", vars.TeamName,
		"{team_description}", vars.TeamDescription,
		"{member_name}", vars.MemberName,
		"{member_description}", vars.MemberDescription,
		"{leader_name}", vars.LeaderName,
	)
	return r.Replace(t.SystemPromptTemplate)
}

// NewAgentFromTemplate creates a UnifiedAgent from a template.
func NewAgentFromTemplate(tmpl SubAgentTemplate, name string, m model.ChatModel, vars *TemplateVars) *agent.UnifiedAgent {
	prompt := tmpl.Render(vars)

	var opts []agent.AgentOption
	if tmpl.ReactConfig != nil {
		opts = append(opts, agent.WithReactConfig(*tmpl.ReactConfig))
	}
	if tmpl.ContextConfig != nil {
		opts = append(opts, agent.WithContextConfig(tmpl.ContextConfig))
	}
	if tmpl.PermissionCtx != nil {
		opts = append(opts, agent.WithPermissionContext(tmpl.PermissionCtx))
	}

	return agent.NewUnifiedAgent(name, prompt, m, opts...)
}

// DefaultTemplate is the default sub-agent template.
var DefaultTemplate = SubAgentTemplate{
	Type:        "default",
	Description: "A general-purpose team member",
	SystemPromptTemplate: "You are {member_name}, a member of team '{team_name}' led by {leader_name}.\n\n" +
		"Team purpose: {team_description}\n\n" +
		"Your role: {member_description}\n\n" +
		"Communicate with the team leader through the team_say tool. " +
		"Focus on your assigned tasks and report results back to the leader.",
}
