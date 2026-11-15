package main

import (
	"context"
	"fmt"
	"time"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/schedule"
)

// This example demonstrates the scheduling system.
// It creates both one-shot and recurring tasks, showing how the scheduler
// manages task lifecycle.

func main() {
	as.Init()

	scheduler := schedule.NewInMemoryScheduler()
	defer scheduler.Close()

	ctx := context.Background()

	// Schedule a one-shot task that runs after 1 second.
	fmt.Println("=== Scheduling Tasks ===")

	id1, _ := scheduler.Schedule(ctx, &schedule.Task{
		Name:        "greeting",
		Description: "Print a greeting after a short delay",
		RunAt:       time.Now().Add(1 * time.Second),
	}, func(ctx context.Context, task *schedule.Task) error {
		fmt.Printf("[%s] One-shot task %q executed!\n", time.Now().Format("15:04:05"), task.Name)
		return nil
	})
	fmt.Printf("Scheduled one-shot task: %s\n", id1)

	// Schedule a recurring task that runs every 2 seconds.
	var count int
	id2, _ := scheduler.Schedule(ctx, &schedule.Task{
		Name:        "heartbeat",
		Description: "Periodic heartbeat check",
		Interval:    2 * time.Second,
	}, func(ctx context.Context, task *schedule.Task) error {
		count++
		fmt.Printf("[%s] Heartbeat #%d\n", time.Now().Format("15:04:05"), count)
		return nil
	})
	fmt.Printf("Scheduled recurring task: %s\n", id2)

	// Schedule a task that will fail.
	id3, _ := scheduler.Schedule(ctx, &schedule.Task{
		Name:        "failing-task",
		Description: "A task that always fails",
		RunAt:       time.Now().Add(500 * time.Millisecond),
	}, func(ctx context.Context, task *schedule.Task) error {
		return fmt.Errorf("simulated failure")
	})
	fmt.Printf("Scheduled failing task: %s\n", id3)

	fmt.Println("\nWaiting for tasks to execute...")
	time.Sleep(5 * time.Second)

	// List all tasks and their status.
	fmt.Println("\n=== Task Status ===")
	tasks, _ := scheduler.List(ctx)
	for _, t := range tasks {
		status := string(t.Status)
		if t.Error != "" {
			status += " (" + t.Error + ")"
		}
		lastRun := "never"
		if !t.LastRunAt.IsZero() {
			lastRun = t.LastRunAt.Format("15:04:05")
		}
		fmt.Printf("  %-15s status=%-12s last_run=%s\n", t.Name, status, lastRun)
	}

	// Cancel the recurring task.
	fmt.Println("\nCanceling heartbeat task...")
	scheduler.Cancel(ctx, id2)

	task, _ := scheduler.Get(ctx, id2)
	fmt.Printf("Heartbeat status: %s\n", task.Status)
}
