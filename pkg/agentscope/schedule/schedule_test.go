package schedule

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestScheduleImmediate(t *testing.T) {
	s := NewInMemoryScheduler()
	defer s.Close()

	var ran atomic.Bool
	id, err := s.Schedule(context.Background(), &Task{Name: "immediate"}, func(ctx context.Context, task *Task) error {
		ran.Store(true)
		return nil
	})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty task ID")
	}

	time.Sleep(100 * time.Millisecond)
	if !ran.Load() {
		t.Error("task should have run immediately")
	}

	task, _ := s.Get(context.Background(), id)
	if task.Status != StatusCompleted {
		t.Errorf("status = %q, want %q", task.Status, StatusCompleted)
	}
}

func TestScheduleDelayed(t *testing.T) {
	s := NewInMemoryScheduler()
	defer s.Close()

	var ran atomic.Bool
	id, _ := s.Schedule(context.Background(), &Task{
		Name:  "delayed",
		RunAt: time.Now().Add(100 * time.Millisecond),
	}, func(ctx context.Context, task *Task) error {
		ran.Store(true)
		return nil
	})

	time.Sleep(50 * time.Millisecond)
	if ran.Load() {
		t.Error("task should not have run yet")
	}

	time.Sleep(100 * time.Millisecond)
	if !ran.Load() {
		t.Error("task should have run after delay")
	}

	task, _ := s.Get(context.Background(), id)
	if task.Status != StatusCompleted {
		t.Errorf("status = %q, want %q", task.Status, StatusCompleted)
	}
}

func TestScheduleRecurring(t *testing.T) {
	s := NewInMemoryScheduler()
	defer s.Close()

	var count atomic.Int32
	_, _ = s.Schedule(context.Background(), &Task{
		Name:     "recurring",
		Interval: 50 * time.Millisecond,
	}, func(ctx context.Context, task *Task) error {
		count.Add(1)
		return nil
	})

	time.Sleep(200 * time.Millisecond)
	n := count.Load()
	if n < 2 {
		t.Errorf("recurring task ran %d times, want at least 2", n)
	}
}

func TestCancel(t *testing.T) {
	s := NewInMemoryScheduler()
	defer s.Close()

	var ran atomic.Bool
	id, _ := s.Schedule(context.Background(), &Task{
		Name:  "cancel-me",
		RunAt: time.Now().Add(time.Hour),
	}, func(ctx context.Context, task *Task) error {
		ran.Store(true)
		return nil
	})

	err := s.Cancel(context.Background(), id)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	task, _ := s.Get(context.Background(), id)
	if task.Status != StatusCanceled {
		t.Errorf("status = %q, want %q", task.Status, StatusCanceled)
	}

	time.Sleep(50 * time.Millisecond)
	if ran.Load() {
		t.Error("canceled task should not run")
	}
}

func TestCancelNotFound(t *testing.T) {
	s := NewInMemoryScheduler()
	defer s.Close()

	err := s.Cancel(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown task")
	}
}

func TestGetNotFound(t *testing.T) {
	s := NewInMemoryScheduler()
	defer s.Close()

	_, err := s.Get(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown task")
	}
}

func TestList(t *testing.T) {
	s := NewInMemoryScheduler()
	defer s.Close()

	noop := func(ctx context.Context, task *Task) error { return nil }
	_, _ = s.Schedule(context.Background(), &Task{Name: "a", RunAt: time.Now().Add(time.Hour)}, noop)
	_, _ = s.Schedule(context.Background(), &Task{Name: "b", RunAt: time.Now().Add(time.Hour)}, noop)

	tasks, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("List = %d, want 2", len(tasks))
	}
}

func TestTaskError(t *testing.T) {
	s := NewInMemoryScheduler()
	defer s.Close()

	id, _ := s.Schedule(context.Background(), &Task{Name: "failing"}, func(ctx context.Context, task *Task) error {
		return fmt.Errorf("boom")
	})

	time.Sleep(100 * time.Millisecond)

	task, _ := s.Get(context.Background(), id)
	if task.Status != StatusFailed {
		t.Errorf("status = %q, want %q", task.Status, StatusFailed)
	}
	if task.Error != "boom" {
		t.Errorf("error = %q, want %q", task.Error, "boom")
	}
}

func TestClose(t *testing.T) {
	s := NewInMemoryScheduler()

	var ran atomic.Bool
	_, _ = s.Schedule(context.Background(), &Task{
		Name:  "close-me",
		RunAt: time.Now().Add(time.Hour),
	}, func(ctx context.Context, task *Task) error {
		ran.Store(true)
		return nil
	})

	s.Close()

	_, err := s.Schedule(context.Background(), &Task{Name: "after-close"}, func(ctx context.Context, task *Task) error {
		return nil
	})
	if err == nil {
		t.Fatal("Schedule after close should fail")
	}

	if ran.Load() {
		t.Error("pending task should not run after close")
	}
}

func TestCloseIdempotent(t *testing.T) {
	s := NewInMemoryScheduler()
	s.Close()
	if err := s.Close(); err != nil {
		t.Errorf("second Close should be no-op, got: %v", err)
	}
}

func TestRecurringLastRunAt(t *testing.T) {
	s := NewInMemoryScheduler()
	defer s.Close()

	id, _ := s.Schedule(context.Background(), &Task{
		Name:     "track-time",
		Interval: 50 * time.Millisecond,
	}, func(ctx context.Context, task *Task) error {
		return nil
	})

	time.Sleep(120 * time.Millisecond)

	task, _ := s.Get(context.Background(), id)
	if task.LastRunAt.IsZero() {
		t.Error("LastRunAt should be set after execution")
	}
}

func TestInMemorySchedulerKeepsCanceledStateAfterCallback(t *testing.T) {
	for _, tc := range []struct {
		name           string
		closeScheduler bool
		callbackErr    error
	}{
		{name: "cancel with nil result"},
		{name: "cancel with error result", callbackErr: context.Canceled},
		{name: "close with nil result", closeScheduler: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewInMemoryScheduler()
			defer s.Close()
			started := make(chan struct{})
			notified := make(chan struct{})
			release := make(chan struct{})
			defer func() {
				if release != nil {
					close(release)
				}
			}()
			finish := release
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			id, err := s.Schedule(ctx, &Task{Name: tc.name}, func(runCtx context.Context, _ *Task) error {
				close(started)
				<-runCtx.Done()
				close(notified)
				<-finish
				return tc.callbackErr
			})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("callback did not start")
			}
			if tc.closeScheduler {
				err = s.Close()
			} else {
				err = s.Cancel(ctx, id)
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-notified:
			case <-ctx.Done():
				t.Fatal("callback did not observe cancellation")
			}
			task, err := s.Get(ctx, id)
			if err != nil || task.Status != StatusCanceled {
				t.Fatalf("before callback exit: task=%+v err=%v, want canceled", task, err)
			}
			close(release)
			release = nil
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				task, err = s.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if !task.LastRunAt.IsZero() {
					if task.Status != StatusCanceled || task.Error != "" {
						t.Fatalf("after callback exit: status=%s, error=%q, want canceled without an error", task.Status, task.Error)
					}
					break
				}
				select {
				case <-tick.C:
				case <-ctx.Done():
					t.Fatal("scheduler did not record callback exit")
				}
			}
		})
	}
}

func TestInMemorySchedulerDoesNotStartCanceledEntry(t *testing.T) {
	s := NewInMemoryScheduler()
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	task := &Task{ID: "due", Status: StatusPending}
	ran := false
	entry := &scheduledEntry{task: task, cancel: cancel, fn: func(context.Context, *Task) error {
		ran = true
		return nil
	}}
	s.tasks[task.ID] = entry
	if err := s.Cancel(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	// Models a timer or ticker selected just before cancellation.
	s.executeOnce(ctx, entry)
	if ran || task.Status != StatusCanceled || !task.LastRunAt.IsZero() {
		t.Fatalf("after a canceled entry was selected: ran=%t, task=%+v", ran, task)
	}
}
