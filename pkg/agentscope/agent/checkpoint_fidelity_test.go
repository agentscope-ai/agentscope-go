package agent

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// Checkpoint fidelity audit: a SaveCheckpoint/LoadCheckpoint round trip must
// preserve every block type a coding session carries — including provider
// extras (Anthropic thinking signatures, OpenAI reasoning item ids) that
// formatters need to pass the blocks back to the model, and the tool-call
// metadata a resumed HITL flow depends on.
func TestCheckpointFidelity_BlockRoundTrip(t *testing.T) {
	saver := newFakeStateSaver()
	a := NewUnifiedAgent("fid-agent", "helpful", &mockChatModel{},
		WithStateSaver(saver),
		WithPermissionContext(permission.NewContext(permission.ModeDefault)),
	)
	a.state.SessionID = "sess-fidelity"
	a.state.Summary = "earlier summary"

	a.state.Context = []*message.Msg{
		{
			Role: message.RoleUser, Name: "user",
			Content: []message.ContentBlock{
				message.TextBlock{Type: "text", Text: "fix the bug"},
			},
		},
		{
			Role: message.RoleAssistant, Name: "fid-agent",
			Content: []message.ContentBlock{
				// Anthropic extended thinking with its signature — dropping
				// Extra makes the resumed conversation invalid to replay.
				message.ThinkingBlock{
					Type:     "thinking",
					Thinking: "let me analyze",
					Extra:    map[string]any{"signature": "sig-abc123"},
				},
				message.RedactedThinkingBlock{Type: "redacted_thinking", Data: "opaque-data"},
				message.ToolCallBlock{
					Type:  "tool_call",
					ID:    "tc_1",
					Name:  "Read",
					Input: `{"file_path":"/x.go"}`,
					Extra: map[string]any{"reasoning_item_id": "rs_789"},
					SuggestedRules: []any{
						permission.Rule{ToolName: "Read", RuleContent: "/x.go", Behavior: permission.BehaviorAllow},
					},
				},
			},
		},
		{
			Role: message.RoleAssistant, Name: "fid-agent",
			Content: []message.ContentBlock{
				message.ToolResultBlock{
					Type:  "tool_result",
					ID:    "tc_1",
					Name:  "Read",
					State: message.ToolResultSuccess,
					Output: []message.ContentBlock{
						message.TextBlock{Type: "text", Text: "package x"},
						message.DataBlock{Type: "data", Source: message.Base64Source{Type: "base64", MediaType: "image/png", Data: "aGk="}},
					},
					Metadata: map[string]any{"diff": "@@ -1 +1 @@"},
				},
			},
		},
	}
	a.state.Plan = []tool.PlanItem{{Step: "fix", Status: tool.PlanStatusInProgress}}
	a.engine.AddWorkingDirectory(permission.WorkingDirectory{Path: "/granted", Source: "granted"})

	if err := a.SaveCheckpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCheckpoint(context.Background(), saver, "sess-fidelity")
	if err != nil {
		t.Fatal(err)
	}

	if loaded.Summary != "earlier summary" {
		t.Errorf("summary lost: %q", loaded.Summary)
	}
	if len(loaded.Context) != 3 {
		t.Fatalf("context length changed: %d", len(loaded.Context))
	}

	// Thinking with extras.
	th, ok := loaded.Context[1].Content[0].(message.ThinkingBlock)
	if !ok {
		t.Fatalf("thinking block type lost: %T", loaded.Context[1].Content[0])
	}
	if th.Thinking != "let me analyze" || th.Extra["signature"] != "sig-abc123" {
		t.Errorf("thinking extra lost: %+v", th)
	}
	// Redacted thinking.
	rd, ok := loaded.Context[1].Content[1].(message.RedactedThinkingBlock)
	if !ok || rd.Data != "opaque-data" {
		t.Errorf("redacted thinking lost: %+v", loaded.Context[1].Content[1])
	}
	// Tool call with extras and suggested rules.
	tc, ok := loaded.Context[1].Content[2].(message.ToolCallBlock)
	if !ok {
		t.Fatalf("tool call type lost: %T", loaded.Context[1].Content[2])
	}
	if tc.Extra["reasoning_item_id"] != "rs_789" {
		t.Errorf("tool call extra lost: %+v", tc.Extra)
	}
	if len(tc.SuggestedRules) != 1 {
		t.Errorf("suggested rules lost: %+v", tc.SuggestedRules)
	} else if rule, ok := tc.SuggestedRules[0].(map[string]any); !ok || rule["tool_name"] != "Read" {
		t.Errorf("suggested rule degraded: %+v", tc.SuggestedRules[0])
	}
	// Tool result with block output, metadata, and an image.
	tr, ok := loaded.Context[2].Content[0].(message.ToolResultBlock)
	if !ok {
		t.Fatalf("tool result type lost: %T", loaded.Context[2].Content[0])
	}
	blocks, ok := tr.Output.([]message.ContentBlock)
	if !ok || len(blocks) != 2 {
		t.Fatalf("block output degraded: %T %+v", tr.Output, tr.Output)
	}
	if tb, ok := blocks[0].(message.TextBlock); !ok || tb.Text != "package x" {
		t.Errorf("text output block degraded: %T %+v", blocks[0], blocks[0])
	}
	db, ok := blocks[1].(message.DataBlock)
	if !ok {
		t.Fatalf("data output block type lost: %T", blocks[1])
	}
	src, ok := db.Source.(message.Base64Source)
	if !ok || src.Data != "aGk=" || src.MediaType != "image/png" {
		t.Errorf("data source degraded: %T %+v", db.Source, db.Source)
	}
	if tr.Metadata["diff"] != "@@ -1 +1 @@" {
		t.Errorf("result metadata lost: %+v", tr.Metadata)
	}
	// Plan and permission grants.
	if len(loaded.Plan) != 1 || loaded.Plan[0].Step != "fix" {
		t.Errorf("plan lost: %+v", loaded.Plan)
	}
	if _, ok := loaded.PermissionCtx.WorkingDirectories["/granted"]; !ok {
		t.Error("working-directory grant lost")
	}
}
