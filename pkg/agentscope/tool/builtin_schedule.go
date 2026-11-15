package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/schedule"
	"github.com/google/uuid"
)

type scheduleContextKey struct{}

// WithScheduler attaches a Scheduler to the context for schedule tools.
func WithScheduler(ctx context.Context, s schedule.Scheduler) context.Context {
	return context.WithValue(ctx, scheduleContextKey{}, s)
}

// GetScheduler retrieves the Scheduler from context.
func GetScheduler(ctx context.Context) schedule.Scheduler {
	v, _ := ctx.Value(scheduleContextKey{}).(schedule.Scheduler)
	return v
}

// --- ScheduleCreateTool ---

type scheduleCreateTool struct{ BaseTool }

var scheduleCreateSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"name":     {"type": "string", "description": "Name for the scheduled task"},
		"input":    {"type": "string", "description": "The input/prompt to execute"},
		"interval": {"type": "string", "description": "Interval like '5m', '1h', '24h'"},
		"run_at":   {"type": "string", "description": "ISO 8601 datetime for one-shot execution"}
	},
	"required": ["name", "input"]
}`)

func (t *scheduleCreateTool) Execute(ctx context.Context, input map[string]any) (*ToolResponse, error) {
	s := GetScheduler(ctx)
	if s == nil {
		return NewErrorResponse(fmt.Errorf("scheduler not available")), nil
	}

	name, _ := input["name"].(string)
	taskInput, _ := input["input"].(string)

	task := &schedule.Task{
		Name:  name,
		Input: taskInput,
	}

	if interval, ok := input["interval"].(string); ok && interval != "" {
		d, err := time.ParseDuration(interval)
		if err != nil {
			return NewErrorResponse(fmt.Errorf("invalid interval: %w", err)), nil
		}
		task.Interval = d
	} else if runAt, ok := input["run_at"].(string); ok && runAt != "" {
		t, err := time.Parse(time.RFC3339, runAt)
		if err != nil {
			return NewErrorResponse(fmt.Errorf("invalid run_at: %w", err)), nil
		}
		task.RunAt = t
	}

	fn := schedule.TaskFunc(func(ctx context.Context, t *schedule.Task) error {
		return nil // actual execution wired by the app layer
	})

	id, err := s.Schedule(ctx, task, fn)
	if err != nil {
		return NewErrorResponse(err), nil
	}

	return NewTextResponse(fmt.Sprintf("Scheduled task %q with ID: %s", name, id)), nil
}

// --- ScheduleDeleteTool ---

type scheduleDeleteTool struct{ BaseTool }

var scheduleDeleteSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"task_id": {"type": "string", "description": "ID of the task to cancel"}
	},
	"required": ["task_id"]
}`)

func (t *scheduleDeleteTool) Execute(ctx context.Context, input map[string]any) (*ToolResponse, error) {
	s := GetScheduler(ctx)
	if s == nil {
		return NewErrorResponse(fmt.Errorf("scheduler not available")), nil
	}
	taskID, _ := input["task_id"].(string)
	if err := s.Cancel(ctx, taskID); err != nil {
		return NewErrorResponse(err), nil
	}
	return NewTextResponse(fmt.Sprintf("Canceled task %s", taskID)), nil
}

// --- ScheduleListTool ---

type scheduleListTool struct{ BaseTool }

var scheduleListSchema = json.RawMessage(`{"type": "object", "properties": {}}`)

func (t *scheduleListTool) Execute(ctx context.Context, _ map[string]any) (*ToolResponse, error) {
	s := GetScheduler(ctx)
	if s == nil {
		return NewErrorResponse(fmt.Errorf("scheduler not available")), nil
	}
	tasks, err := s.List(ctx)
	if err != nil {
		return NewErrorResponse(err), nil
	}
	data, _ := json.Marshal(tasks)
	return NewTextResponse(string(data)), nil
}

// --- ScheduleViewTool ---

type scheduleViewTool struct{ BaseTool }

var scheduleViewSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"task_id": {"type": "string", "description": "ID of the task to view"}
	},
	"required": ["task_id"]
}`)

func (t *scheduleViewTool) Execute(ctx context.Context, input map[string]any) (*ToolResponse, error) {
	s := GetScheduler(ctx)
	if s == nil {
		return NewErrorResponse(fmt.Errorf("scheduler not available")), nil
	}
	taskID, _ := input["task_id"].(string)
	task, err := s.Get(ctx, taskID)
	if err != nil {
		return NewErrorResponse(err), nil
	}
	data, _ := json.Marshal(task)
	return NewTextResponse(string(data)), nil
}

// NewScheduleTools creates the set of schedule-related tools.
func NewScheduleTools() []Tool {
	_ = uuid.New // ensure import
	return []Tool{
		&scheduleCreateTool{BaseTool: BaseTool{
			ToolName:        "ScheduleCreate",
			ToolDescription: "Create a new scheduled task to run at a specific time or interval",
			ToolSchema:      scheduleCreateSchema,
			ConcurrencySafe: true,
		}},
		&scheduleDeleteTool{BaseTool: BaseTool{
			ToolName:        "ScheduleDelete",
			ToolDescription: "Cancel a scheduled task",
			ToolSchema:      scheduleDeleteSchema,
			ConcurrencySafe: true,
		}},
		&scheduleListTool{BaseTool: BaseTool{
			ToolName:        "ScheduleList",
			ToolDescription: "List all scheduled tasks",
			ToolSchema:      scheduleListSchema,
			ConcurrencySafe: true,
			ReadOnly:        true,
		}},
		&scheduleViewTool{BaseTool: BaseTool{
			ToolName:        "ScheduleView",
			ToolDescription: "View details of a specific scheduled task",
			ToolSchema:      scheduleViewSchema,
			ConcurrencySafe: true,
			ReadOnly:        true,
		}},
	}
}
