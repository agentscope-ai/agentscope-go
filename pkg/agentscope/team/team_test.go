package team

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func TestNewTeam(t *testing.T) {
	tm := NewTeam("alpha", "Research team", "leader")
	if tm.Name != "alpha" {
		t.Errorf("Name = %s, want alpha", tm.Name)
	}
	if tm.LeaderName != "leader" {
		t.Errorf("LeaderName = %s", tm.LeaderName)
	}
	if tm.ID == "" {
		t.Error("ID should not be empty")
	}
}

func TestTeam_AddMember(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")

	m, err := tm.AddMember("worker1", "does work")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "worker1" {
		t.Errorf("Name = %s", m.Name)
	}

	if len(tm.Members()) != 1 {
		t.Errorf("Members = %d, want 1", len(tm.Members()))
	}
}

func TestTeam_AddMember_DuplicateName(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")
	_, _ = tm.AddMember("worker1", "first")

	_, err := tm.AddMember("worker1", "second")
	if err == nil {
		t.Error("should reject duplicate name")
	}
}

func TestTeam_AddMember_LeaderNameConflict(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")

	_, err := tm.AddMember("leader", "tries to impersonate")
	if err == nil {
		t.Error("should reject name matching leader")
	}
}

func TestTeam_RemoveMember(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")
	_, _ = tm.AddMember("worker1", "temp")

	err := tm.RemoveMember("worker1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tm.Members()) != 0 {
		t.Error("member should be removed")
	}
}

func TestTeam_RemoveMember_NotFound(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")
	err := tm.RemoveMember("ghost")
	if err == nil {
		t.Error("should error on unknown member")
	}
}

func TestTeam_Send(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")
	_, _ = tm.AddMember("worker1", "listener")

	err := tm.Send("leader", "worker1", "do this task")
	if err != nil {
		t.Fatal(err)
	}

	m := tm.GetMember("worker1")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	msg, err := m.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	txt := msg.GetTextContent("\n")
	if txt == nil || !strings.Contains(*txt, "do this task") {
		t.Errorf("unexpected message: %v", txt)
	}
}

func TestTeam_Send_ToLeader(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")
	_, _ = tm.AddMember("worker1", "reporter")

	err := tm.Send("worker1", "leader", "done")
	if err != ErrLeaderRecipient {
		t.Errorf("expected ErrLeaderRecipient, got %v", err)
	}
}

func TestTeam_Send_ToSelf(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")
	_, _ = tm.AddMember("worker1", "lonely")

	err := tm.Send("worker1", "worker1", "hello me")
	if err == nil {
		t.Error("should reject self-messages")
	}
}

func TestTeam_Broadcast(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")
	_, _ = tm.AddMember("w1", "first")
	_, _ = tm.AddMember("w2", "second")

	err := tm.Broadcast("leader", "announcement")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	for _, name := range []string{"w1", "w2"} {
		m := tm.GetMember(name)
		msg, err := m.Receive(ctx)
		if err != nil {
			t.Fatalf("member %s: %v", name, err)
		}
		txt := msg.GetTextContent("\n")
		if txt == nil || !strings.Contains(*txt, "announcement") {
			t.Errorf("member %s: unexpected message: %v", name, txt)
		}
	}
}

func TestTeam_Disband(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")
	_, _ = tm.AddMember("w1", "")
	_, _ = tm.AddMember("w2", "")

	tm.Disband()

	if len(tm.Members()) != 0 {
		t.Error("should have no members after disband")
	}
}

func TestTeam_ContextInjection(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")
	ctx := WithTeam(context.Background(), tm)

	got := GetTeam(ctx)
	if got != tm {
		t.Error("should retrieve same team from context")
	}

	if GetTeam(context.Background()) != nil {
		t.Error("empty context should return nil")
	}
}

// --- Template tests ---

func TestSubAgentTemplate_Render(t *testing.T) {
	tmpl := DefaultTemplate
	result := tmpl.Render(&TemplateVars{
		TeamName:          "Research Squad",
		TeamDescription:   "We do research",
		MemberName:        "Alice",
		MemberDescription: "Researcher",
		LeaderName:        "Bob",
	})

	if !strings.Contains(result, "Alice") {
		t.Error("should contain member name")
	}
	if !strings.Contains(result, "Research Squad") {
		t.Error("should contain team name")
	}
	if !strings.Contains(result, "Bob") {
		t.Error("should contain leader name")
	}
}

// --- Tool tests ---

func TestTeamCreateTool(t *testing.T) {
	tc := TeamCreateTool("leader")
	resp, err := tc.Execute(context.Background(), map[string]any{
		"name":        "alpha",
		"description": "test team",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "alpha") {
		t.Errorf("unexpected response: %s", text)
	}
}

func TestTeamCreateTool_AlreadyInTeam(t *testing.T) {
	tm := NewTeam("existing", "desc", "leader")
	ctx := WithTeam(context.Background(), tm)

	tc := TeamCreateTool("leader")
	resp, _ := tc.Execute(ctx, map[string]any{
		"name":        "second",
		"description": "another",
	})
	text := getResponseText(resp)
	if !strings.Contains(text, "already") {
		t.Errorf("should reject when already in team: %s", text)
	}
}

func TestAgentCreateTool(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")
	ctx := WithTeam(context.Background(), tm)

	ac := AgentCreateTool("leader")
	resp, err := ac.Execute(ctx, map[string]any{
		"name":        "worker1",
		"description": "helper",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "worker1") {
		t.Errorf("unexpected response: %s", text)
	}

	if len(tm.Members()) != 1 {
		t.Errorf("should have 1 member, got %d", len(tm.Members()))
	}
}

func TestAgentCreateTool_NotLeader(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")
	ctx := WithTeam(context.Background(), tm)

	ac := AgentCreateTool("worker1")
	resp, _ := ac.Execute(ctx, map[string]any{
		"name":        "worker2",
		"description": "helper",
	})
	text := getResponseText(resp)
	if !strings.Contains(text, "leader") {
		t.Errorf("should reject non-leader: %s", text)
	}
}

func TestTeamSayTool_Send(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")
	_, _ = tm.AddMember("worker1", "listener")
	ctx := WithTeam(context.Background(), tm)

	say := TeamSayTool("leader")
	resp, err := say.Execute(ctx, map[string]any{
		"content": "hello worker",
		"to":      "worker1",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "sent") {
		t.Errorf("unexpected response: %s", text)
	}
}

func TestTeamSayTool_Broadcast(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")
	_, _ = tm.AddMember("w1", "")
	_, _ = tm.AddMember("w2", "")
	ctx := WithTeam(context.Background(), tm)

	say := TeamSayTool("leader")
	resp, _ := say.Execute(ctx, map[string]any{
		"content": "all hands",
	})
	text := getResponseText(resp)
	if !strings.Contains(text, "broadcast") {
		t.Errorf("unexpected response: %s", text)
	}
}

func TestTeamDeleteTool(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")
	_, _ = tm.AddMember("w1", "")
	ctx := WithTeam(context.Background(), tm)

	del := TeamDeleteTool("leader")
	resp, err := del.Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "disbanded") {
		t.Errorf("unexpected response: %s", text)
	}
	if len(tm.Members()) != 0 {
		t.Error("should have no members after delete")
	}
}

func TestTeamDeleteTool_NotLeader(t *testing.T) {
	tm := NewTeam("alpha", "desc", "leader")
	ctx := WithTeam(context.Background(), tm)

	del := TeamDeleteTool("worker1")
	resp, _ := del.Execute(ctx, map[string]any{})
	text := getResponseText(resp)
	if !strings.Contains(text, "leader") {
		t.Errorf("should reject non-leader: %s", text)
	}
}

func TestLeaderToolkit(t *testing.T) {
	tk := NewLeaderToolkit("leader")
	schemas := tk.GetToolSchemas()
	if len(schemas) != 4 {
		t.Errorf("leader toolkit should have 4 tools, got %d", len(schemas))
	}
}

func TestMemberToolkit(t *testing.T) {
	tk := NewMemberToolkit("worker1")
	schemas := tk.GetToolSchemas()
	if len(schemas) != 1 {
		t.Errorf("member toolkit should have 1 tool, got %d", len(schemas))
	}
}

func getResponseText(resp *tool.ToolResponse) string {
	if resp == nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range resp.Content {
		if tb, ok := b.(message.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}
