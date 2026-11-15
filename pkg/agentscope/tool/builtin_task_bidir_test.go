package tool

import (
	"context"
	"testing"
)

func TestTaskUpdate_BidirectionalBlocks(t *testing.T) {
	tc := NewTaskContext()
	ctx := WithTaskContext(context.Background(), tc)

	// Create tasks
	tc.create("Task A", "desc A")
	tc.create("Task B", "desc B")
	tc.create("Task C", "desc C")

	tool := TaskUpdateTool()

	// Set task 1 blocks task 2 and 3
	resp, _ := tool.Execute(ctx, map[string]any{
		"task_id": "1",
		"blocks":  []any{"2", "3"},
	})
	if resp.State != "success" {
		t.Fatalf("update failed: %s", getResponseText(resp))
	}

	// Check that task 1 has blocks [2, 3]
	task1 := tc.get("1")
	if len(task1.Blocks) != 2 || task1.Blocks[0] != "2" || task1.Blocks[1] != "3" {
		t.Fatalf("task1.Blocks = %v, want [2, 3]", task1.Blocks)
	}

	// Check that task 2 has blocked_by [1]
	task2 := tc.get("2")
	if len(task2.BlockedBy) != 1 || task2.BlockedBy[0] != "1" {
		t.Fatalf("task2.BlockedBy = %v, want [1]", task2.BlockedBy)
	}

	// Check that task 3 has blocked_by [1]
	task3 := tc.get("3")
	if len(task3.BlockedBy) != 1 || task3.BlockedBy[0] != "1" {
		t.Fatalf("task3.BlockedBy = %v, want [1]", task3.BlockedBy)
	}
}

func TestTaskUpdate_BidirectionalBlockedBy(t *testing.T) {
	tc := NewTaskContext()
	ctx := WithTaskContext(context.Background(), tc)

	tc.create("Task A", "desc A")
	tc.create("Task B", "desc B")

	tool := TaskUpdateTool()

	// Set task 2 blocked_by task 1
	resp, _ := tool.Execute(ctx, map[string]any{
		"task_id":    "2",
		"blocked_by": []any{"1"},
	})
	if resp.State != "success" {
		t.Fatalf("update failed: %s", getResponseText(resp))
	}

	// Check that task 2 has blocked_by [1]
	task2 := tc.get("2")
	if len(task2.BlockedBy) != 1 || task2.BlockedBy[0] != "1" {
		t.Fatalf("task2.BlockedBy = %v, want [1]", task2.BlockedBy)
	}

	// Check that task 1 has blocks [2] (bidirectional)
	task1 := tc.get("1")
	if len(task1.Blocks) != 1 || task1.Blocks[0] != "2" {
		t.Fatalf("task1.Blocks = %v, want [2]", task1.Blocks)
	}
}

func TestTaskUpdate_BidirectionalUpdate(t *testing.T) {
	tc := NewTaskContext()
	ctx := WithTaskContext(context.Background(), tc)

	tc.create("Task A", "desc A")
	tc.create("Task B", "desc B")
	tc.create("Task C", "desc C")

	tool := TaskUpdateTool()

	// Set task 1 blocks 2
	_, _ = tool.Execute(ctx, map[string]any{
		"task_id": "1",
		"blocks":  []any{"2"},
	})

	// Verify task2.BlockedBy is [1]
	if task := tc.get("2"); len(task.BlockedBy) != 1 || task.BlockedBy[0] != "1" {
		t.Fatalf("task2.BlockedBy = %v, want [1]", task.BlockedBy)
	}

	// Now update task 1 to block 3 instead of 2
	_, _ = tool.Execute(ctx, map[string]any{
		"task_id": "1",
		"blocks":  []any{"3"},
	})

	// Task 2 should no longer be blocked by 1
	task2 := tc.get("2")
	if len(task2.BlockedBy) != 0 {
		t.Fatalf("task2.BlockedBy = %v, want empty", task2.BlockedBy)
	}

	// Task 3 should now be blocked by 1
	task3 := tc.get("3")
	if len(task3.BlockedBy) != 1 || task3.BlockedBy[0] != "1" {
		t.Fatalf("task3.BlockedBy = %v, want [1]", task3.BlockedBy)
	}

	// Task 1 should block 3
	task1 := tc.get("1")
	if len(task1.Blocks) != 1 || task1.Blocks[0] != "3" {
		t.Fatalf("task1.Blocks = %v, want [3]", task1.Blocks)
	}
}

func TestTaskUpdate_NoDuplicates(t *testing.T) {
	tc := NewTaskContext()
	ctx := WithTaskContext(context.Background(), tc)

	tc.create("Task A", "desc A")
	tc.create("Task B", "desc B")

	tool := TaskUpdateTool()

	// Set blocks twice - should not create duplicates
	_, _ = tool.Execute(ctx, map[string]any{
		"task_id": "1",
		"blocks":  []any{"2"},
	})
	_, _ = tool.Execute(ctx, map[string]any{
		"task_id": "1",
		"blocks":  []any{"2"},
	})

	task2 := tc.get("2")
	if len(task2.BlockedBy) != 1 {
		t.Fatalf("task2.BlockedBy should have exactly 1 entry, got %d: %v", len(task2.BlockedBy), task2.BlockedBy)
	}
}
