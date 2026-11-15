package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Task represents a trackable unit of work.
type Task struct {
	ID          string   `json:"id"`
	Subject     string   `json:"subject"`
	Description string   `json:"description"`
	State       string   `json:"state"` // pending, in_progress, completed
	Owner       string   `json:"owner,omitempty"`
	Blocks      []string `json:"blocks,omitempty"`
	BlockedBy   []string `json:"blocked_by,omitempty"`
	CreatedAt   string   `json:"created_at"`
}

// TaskContext holds tasks for an agent session.
type TaskContext struct {
	mu     sync.RWMutex
	tasks  []*Task
	nextID int
}

// NewTaskContext creates an empty task context.
func NewTaskContext() *TaskContext {
	return &TaskContext{nextID: 1}
}

func (tc *TaskContext) create(subject, description string) *Task {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	t := &Task{
		ID:          strconv.Itoa(tc.nextID),
		Subject:     subject,
		Description: description,
		State:       "pending",
		CreatedAt:   time.Now().Format(time.RFC3339),
	}
	tc.nextID++
	tc.tasks = append(tc.tasks, t)
	return t
}

func (tc *TaskContext) get(id string) *Task {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	for _, t := range tc.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (tc *TaskContext) list() []*Task {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	out := make([]*Task, len(tc.tasks))
	copy(out, tc.tasks)
	return out
}

// findLocked returns a task by ID. Must be called with tc.mu held.
func (tc *TaskContext) findLocked(id string) *Task {
	for _, t := range tc.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (tc *TaskContext) update(id string, updates map[string]any) (*Task, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	t := tc.findLocked(id)
	if t == nil {
		return nil, fmt.Errorf("task %q not found", id)
	}

	if v, ok := updates["state"].(string); ok {
		switch v {
		case "pending", "in_progress", "completed":
			t.State = v
		default:
			return nil, fmt.Errorf("invalid state: %q", v)
		}
	}
	if v, ok := updates["owner"].(string); ok {
		t.Owner = v
	}
	if v, ok := updates["subject"].(string); ok {
		t.Subject = v
	}
	if v, ok := updates["description"].(string); ok {
		t.Description = v
	}

	// Bidirectional dependency updates
	if v, ok := updates["blocks"]; ok {
		newBlocks := toStringSlice(v)
		// Remove this task from old blocked_by entries
		for _, oldBlockedID := range t.Blocks {
			if other := tc.findLocked(oldBlockedID); other != nil {
				other.BlockedBy = removeFromSlice(other.BlockedBy, id)
			}
		}
		t.Blocks = newBlocks
		// Add this task to new blocked_by entries
		for _, blockedID := range newBlocks {
			if other := tc.findLocked(blockedID); other != nil {
				if !containsString(other.BlockedBy, id) {
					other.BlockedBy = append(other.BlockedBy, id)
				}
			}
		}
	}

	if v, ok := updates["blocked_by"]; ok {
		newBlockedBy := toStringSlice(v)
		// Remove this task from old blocks entries
		for _, oldBlockerID := range t.BlockedBy {
			if other := tc.findLocked(oldBlockerID); other != nil {
				other.Blocks = removeFromSlice(other.Blocks, id)
			}
		}
		t.BlockedBy = newBlockedBy
		// Add this task to new blocks entries
		for _, blockerID := range newBlockedBy {
			if other := tc.findLocked(blockerID); other != nil {
				if !containsString(other.Blocks, id) {
					other.Blocks = append(other.Blocks, id)
				}
			}
		}
	}

	return t, nil
}

// removeFromSlice removes all occurrences of target from s.
func removeFromSlice(s []string, target string) []string {
	var result []string
	for _, v := range s {
		if v != target {
			result = append(result, v)
		}
	}
	return result
}

// containsString checks if s contains target.
func containsString(s []string, target string) bool {
	for _, v := range s {
		if v == target {
			return true
		}
	}
	return false
}

// --- Task tools ---

type taskContextKey struct{}

// WithTaskContext attaches a TaskContext to a Go context.
func WithTaskContext(ctx context.Context, tc *TaskContext) context.Context {
	return context.WithValue(ctx, taskContextKey{}, tc)
}

// GetTaskContext retrieves TaskContext from a Go context.
func GetTaskContext(ctx context.Context) *TaskContext {
	tc, _ := ctx.Value(taskContextKey{}).(*TaskContext)
	return tc
}

// --- TaskCreate ---

var taskCreateSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"subject": {"type": "string", "description": "Brief title for the task"},
		"description": {"type": "string", "description": "What needs to be done"}
	},
	"required": ["subject", "description"]
}`)

type taskCreateTool struct{ BaseTool }

func (t *taskCreateTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	tc := GetTaskContext(ctx)
	if tc == nil {
		return NewErrorResponse(fmt.Errorf("no task context available")), nil
	}
	subject, _ := args["subject"].(string)
	desc, _ := args["description"].(string)
	if subject == "" {
		return NewErrorResponse(fmt.Errorf("subject is required")), nil
	}
	task := tc.create(subject, desc)
	b, _ := json.Marshal(task)
	return NewTextResponse(string(b)), nil
}

// TaskCreateTool returns a tool that creates tasks.
func TaskCreateTool() Tool {
	return &taskCreateTool{BaseTool: BaseTool{
		ToolName:        "task_create",
		ToolDescription: "Create a new task to track work. Returns the created task with its ID.",
		ToolSchema:      taskCreateSchema,
	}}
}

// --- TaskGet ---

var taskGetSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"task_id": {"type": "string", "description": "The ID of the task to retrieve"}
	},
	"required": ["task_id"]
}`)

type taskGetTool struct{ BaseTool }

func (t *taskGetTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	tc := GetTaskContext(ctx)
	if tc == nil {
		return NewErrorResponse(fmt.Errorf("no task context available")), nil
	}
	id, _ := args["task_id"].(string)
	if id == "" {
		return NewErrorResponse(fmt.Errorf("task_id is required")), nil
	}
	task := tc.get(id)
	if task == nil {
		return NewErrorResponse(fmt.Errorf("task %q not found", id)), nil
	}
	b, _ := json.Marshal(task)
	return NewTextResponse(string(b)), nil
}

// TaskGetTool returns a tool that retrieves a task by ID.
func TaskGetTool() Tool {
	return &taskGetTool{BaseTool: BaseTool{
		ToolName:        "task_get",
		ToolDescription: "Get a task's details by its ID.",
		ToolSchema:      taskGetSchema,
		ReadOnly:        true,
		ConcurrencySafe: true,
	}}
}

// --- TaskList ---

var taskListSchema = json.RawMessage(`{
	"type": "object",
	"properties": {}
}`)

type taskListTool struct{ BaseTool }

func (t *taskListTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	tc := GetTaskContext(ctx)
	if tc == nil {
		return NewErrorResponse(fmt.Errorf("no task context available")), nil
	}
	tasks := tc.list()
	b, _ := json.Marshal(tasks)
	return NewTextResponse(string(b)), nil
}

// TaskListTool returns a tool that lists all tasks.
func TaskListTool() Tool {
	return &taskListTool{BaseTool: BaseTool{
		ToolName:        "task_list",
		ToolDescription: "List all tasks with their current status.",
		ToolSchema:      taskListSchema,
		ReadOnly:        true,
		ConcurrencySafe: true,
	}}
}

// --- TaskUpdate ---

var taskUpdateSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"task_id": {"type": "string", "description": "The ID of the task to update"},
		"state": {"type": "string", "description": "New state: pending, in_progress, or completed"},
		"owner": {"type": "string", "description": "New owner"},
		"subject": {"type": "string", "description": "New subject"},
		"description": {"type": "string", "description": "New description"},
		"blocks": {
			"type": "array",
			"items": {"type": "string"},
			"description": "Task IDs that this task blocks (bidirectionally updates blocked_by on those tasks)"
		},
		"blocked_by": {
			"type": "array",
			"items": {"type": "string"},
			"description": "Task IDs that block this task (bidirectionally updates blocks on those tasks)"
		}
	},
	"required": ["task_id"]
}`)

type taskUpdateTool struct{ BaseTool }

func (t *taskUpdateTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	tc := GetTaskContext(ctx)
	if tc == nil {
		return NewErrorResponse(fmt.Errorf("no task context available")), nil
	}
	id, _ := args["task_id"].(string)
	if id == "" {
		return NewErrorResponse(fmt.Errorf("task_id is required")), nil
	}
	task, err := tc.update(id, args)
	if err != nil {
		return NewErrorResponse(err), nil
	}
	b, _ := json.Marshal(task)
	return NewTextResponse(string(b)), nil
}

// TaskUpdateTool returns a tool that updates task properties.
func TaskUpdateTool() Tool {
	return &taskUpdateTool{BaseTool: BaseTool{
		ToolName:        "task_update",
		ToolDescription: "Update a task's state, owner, subject, description, blocks, or blocked_by.",
		ToolSchema:      taskUpdateSchema,
	}}
}

// NewTaskToolkit returns a Toolkit with all task management tools.
func NewTaskToolkit() *Toolkit {
	return NewToolkit(TaskCreateTool(), TaskGetTool(), TaskListTool(), TaskUpdateTool())
}
