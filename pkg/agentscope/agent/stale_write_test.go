package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func fileCall(id, name string, args map[string]any) message.ToolCallBlock {
	raw, _ := json.Marshal(args)
	return message.ToolCallBlock{Type: "tool_call", ID: id, Name: name, Input: string(raw)}
}

// lastToolText is the text of the newest tool result the model was sent.
func lastToolText(msgs []*message.Msg) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		// One agent message holds a round's calls and results in order.
		for j := len(msgs[i].Content) - 1; j >= 0; j-- {
			if r, ok := msgs[i].Content[j].(message.ToolResultBlock); ok {
				return r.GetOutputText()
			}
		}
	}
	return ""
}

func staleAgent(name string, m model.ChatModel) *UnifiedAgent {
	rc := tool.NewReadCache(0, 0)
	rc.SetRejectStaleWrites(true)
	return NewUnifiedAgent(name, "sys", m, WithReadCache(rc), WithToolkit(tool.NewToolkit(tool.ReadTool(), tool.WriteTool())))
}

// TestSharedWorkspaceStaleWriteThroughTheAgent: two agents with their own
// read caches share a directory. The cloud agent reads a file, the edge
// agent rewrites it, and the cloud agent's whole-file write from its old
// copy is refused through its toolkit; it reads again and writes over the
// edge's version.
func TestSharedWorkspaceStaleWriteThroughTheAgent(t *testing.T) {
	f := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(f, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edge := staleAgent("edge", &seamModel{respond: func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		switch call {
		case 0:
			return seamToolCalls(fileCall("e1", "Read", map[string]any{"file_path": f})), nil
		case 1:
			return seamToolCalls(fileCall("e2", "Write", map[string]any{"file_path": f, "content": "package main\n\nfunc edge() {}\n"})), nil
		}
		return seamText("edge done"), nil
	}})

	var refusal, reread string
	cloud := staleAgent("cloud", &seamModel{respond: func(call int, msgs []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		switch call {
		case 0:
			return seamToolCalls(fileCall("c1", "Read", map[string]any{"file_path": f})), nil
		case 1:
			// The edge works on the same file meanwhile.
			if _, err := edge.Reply(context.Background(), "add edge()"); err != nil {
				return nil, err
			}
			return seamToolCalls(fileCall("c2", "Write", map[string]any{"file_path": f, "content": "package main\n\nfunc cloud() {}\n"})), nil
		case 2:
			refusal = lastToolText(msgs)
			return seamToolCalls(fileCall("c3", "Read", map[string]any{"file_path": f})), nil
		case 3:
			reread = lastToolText(msgs)
			return seamToolCalls(fileCall("c4", "Write", map[string]any{"file_path": f, "content": "package main\n\nfunc edge() {}\n\nfunc cloud() {}\n"})), nil
		}
		return seamText("cloud done"), nil
	}})

	if _, err := cloud.Reply(context.Background(), "add cloud()"); err != nil {
		t.Fatalf("cloud reply: %v", err)
	}
	if !strings.Contains(refusal, "changed since you last read it") {
		t.Fatalf("the cloud's stale write was not refused: %q", refusal)
	}
	if !strings.Contains(reread, "func edge()") {
		t.Fatalf("the cloud's second read did not return the edge's version: %q", reread)
	}
	data, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, "func edge()") || !strings.Contains(got, "func cloud()") {
		t.Fatalf("final content = %q", got)
	}
}
