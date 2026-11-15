package app

import (
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/workspace"
)

func TestWorkspaceAgentFactory_ReceivesSessionWorkspace(t *testing.T) {
	baseDir := t.TempDir()

	var gotBasePath string
	wsFactory := func(session *SessionRecord, ws workspace.Workspace) (*agent.UnifiedAgent, error) {
		gotBasePath = ws.BasePath()
		return agent.NewUnifiedAgent(session.AgentName, session.SystemPrompt, &mockChatModel{}), nil
	}

	app, err := CreateApp(&AppConfig{
		WorkspaceDir:          baseDir,
		WorkspaceAgentFactory: wsFactory,
	})
	if err != nil {
		t.Fatal(err)
	}

	session := app.sessionSvc.Create(CreateSessionRequest{AgentName: "mem-agent"})
	if _, err := app.sessionSvc.GetOrCreateAgent(session.ID); err != nil {
		t.Fatal(err)
	}
	if gotBasePath == "" {
		t.Fatal("workspace factory must be called")
	}
	if !strings.HasPrefix(gotBasePath, baseDir) || !strings.Contains(gotBasePath, session.ID) {
		t.Errorf("workspace must be the session's, got %q", gotBasePath)
	}
}

func TestWorkspaceAgentFactory_RequiresWorkspaceDir(t *testing.T) {
	_, err := CreateApp(&AppConfig{
		WorkspaceAgentFactory: func(_ *SessionRecord, _ workspace.Workspace) (*agent.UnifiedAgent, error) {
			return nil, nil
		},
	})
	if err == nil {
		t.Error("WorkspaceAgentFactory without WorkspaceDir must error")
	}
}
