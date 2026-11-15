package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestTaskCreate(t *testing.T) {
	tc := NewTaskContext()
	ctx := WithTaskContext(context.Background(), tc)

	tool := TaskCreateTool()
	resp, err := tool.Execute(ctx, map[string]any{
		"subject":     "Fix bug",
		"description": "Fix the login bug",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %s, text = %q", resp.State, getResponseText(resp))
	}

	var task Task
	_ = json.Unmarshal([]byte(getResponseText(resp)), &task)
	if task.ID != "1" {
		t.Fatalf("ID = %q, want '1'", task.ID)
	}
	if task.Subject != "Fix bug" {
		t.Fatalf("Subject = %q", task.Subject)
	}
	if task.State != "pending" {
		t.Fatalf("State = %q, want 'pending'", task.State)
	}
}

func TestTaskCreate_NoContext(t *testing.T) {
	tool := TaskCreateTool()
	resp, _ := tool.Execute(context.Background(), map[string]any{
		"subject":     "test",
		"description": "test",
	})
	if resp.State != message.ToolResultError {
		t.Fatal("expected error without task context")
	}
}

func TestTaskGet(t *testing.T) {
	tc := NewTaskContext()
	ctx := WithTaskContext(context.Background(), tc)
	tc.create("Task A", "Desc A")

	tool := TaskGetTool()
	resp, _ := tool.Execute(ctx, map[string]any{"task_id": "1"})
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %s", resp.State)
	}
	if !strings.Contains(getResponseText(resp), "Task A") {
		t.Fatalf("should contain task subject")
	}

	// Not found
	resp, _ = tool.Execute(ctx, map[string]any{"task_id": "99"})
	if resp.State != message.ToolResultError {
		t.Fatal("expected error for missing task")
	}
}

func TestTaskList(t *testing.T) {
	tc := NewTaskContext()
	ctx := WithTaskContext(context.Background(), tc)
	tc.create("A", "desc a")
	tc.create("B", "desc b")

	tool := TaskListTool()
	resp, _ := tool.Execute(ctx, map[string]any{})
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %s", resp.State)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "A") || !strings.Contains(text, "B") {
		t.Fatalf("missing tasks in list: %q", text)
	}
}

func TestTaskUpdate(t *testing.T) {
	tc := NewTaskContext()
	ctx := WithTaskContext(context.Background(), tc)
	tc.create("Task", "desc")

	tool := TaskUpdateTool()
	resp, _ := tool.Execute(ctx, map[string]any{
		"task_id": "1",
		"state":   "in_progress",
		"owner":   "agent-1",
	})
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %s, text = %q", resp.State, getResponseText(resp))
	}

	task := tc.get("1")
	if task.State != "in_progress" {
		t.Fatalf("state = %q, want 'in_progress'", task.State)
	}
	if task.Owner != "agent-1" {
		t.Fatalf("owner = %q, want 'agent-1'", task.Owner)
	}

	// Invalid state
	resp, _ = tool.Execute(ctx, map[string]any{"task_id": "1", "state": "invalid"})
	if resp.State != message.ToolResultError {
		t.Fatal("expected error for invalid state")
	}

	// Not found
	resp, _ = tool.Execute(ctx, map[string]any{"task_id": "99", "state": "completed"})
	if resp.State != message.ToolResultError {
		t.Fatal("expected error for missing task")
	}
}

func TestResetTools(t *testing.T) {
	tk := NewToolkit()
	schema := json.RawMessage(`{"type":"object","properties":{}}`)
	t1 := NewFunctionTool("t1", "d1", schema, func(ctx context.Context, input map[string]any) (any, error) { return "ok", nil })
	tk.AddGroup("extra", t1)

	rt := ResetToolsTool(tk)
	ctx := context.Background()

	// Deactivate
	resp, _ := rt.Execute(ctx, map[string]any{"deactivate": []any{"extra"}})
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %s", resp.State)
	}
	if tk.Get("t1") != nil {
		t.Fatal("t1 should be deactivated")
	}

	// Activate
	resp, _ = rt.Execute(ctx, map[string]any{"activate": []any{"extra"}})
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %s", resp.State)
	}
	if tk.Get("t1") == nil {
		t.Fatal("t1 should be reactivated")
	}

	// No changes
	resp, _ = rt.Execute(ctx, map[string]any{})
	text := getResponseText(resp)
	if !strings.Contains(text, "No changes") {
		t.Fatalf("expected no changes message: %q", text)
	}
}
