package service

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// TaskStatus represents the lifecycle state of a background task.
type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskRunning   TaskStatus = "running"
	TaskCompleted TaskStatus = "completed"
	TaskCanceled  TaskStatus = "canceled"
	TaskFailed    TaskStatus = "failed"
)

// BackgroundTask tracks a background operation.
type BackgroundTask struct {
	ID        string
	Name      string
	Status    TaskStatus
	Error     error
	CreatedAt time.Time
	DoneAt    time.Time

	cancel context.CancelFunc
}

// BackgroundTaskManager manages the lifecycle of background tasks.
type BackgroundTaskManager struct {
	mu    sync.RWMutex
	tasks map[string]*BackgroundTask
	maxID int
}

// NewBackgroundTaskManager creates a new task manager.
func NewBackgroundTaskManager() *BackgroundTaskManager {
	return &BackgroundTaskManager{
		tasks: make(map[string]*BackgroundTask),
	}
}

// Submit submits a function to run in the background and returns its task ID.
func (m *BackgroundTaskManager) Submit(name string, fn func(ctx context.Context) error) string {
	m.mu.Lock()
	m.maxID++
	id := fmt.Sprintf("bg_%d", m.maxID)
	ctx, cancel := context.WithCancel(context.Background())
	task := &BackgroundTask{
		ID:        id,
		Name:      name,
		Status:    TaskRunning,
		CreatedAt: time.Now(),
		cancel:    cancel,
	}
	m.tasks[id] = task
	m.mu.Unlock()

	go func() {
		err := fn(ctx)

		m.mu.Lock()
		defer m.mu.Unlock()
		task.DoneAt = time.Now()
		if ctx.Err() == context.Canceled {
			task.Status = TaskCanceled
		} else if err != nil {
			task.Status = TaskFailed
			task.Error = err
		} else {
			task.Status = TaskCompleted
		}
	}()

	return id
}

// Cancel cancels a running task.
func (m *BackgroundTaskManager) Cancel(id string) error {
	m.mu.RLock()
	task, ok := m.tasks[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("task %q not found", id)
	}
	if task.cancel != nil {
		task.cancel()
	}
	return nil
}

// Get returns a snapshot of the task with the given ID. The returned value is a
// copy taken under the read lock, so reading its fields cannot race the Submit
// goroutine that mutates Status/Error/DoneAt.
func (m *BackgroundTaskManager) Get(id string) (*BackgroundTask, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, false
	}
	cp := *t
	return &cp, true
}

// List returns snapshots of all tasks (see Get for why copies are returned).
func (m *BackgroundTaskManager) List() []*BackgroundTask {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*BackgroundTask, 0, len(m.tasks))
	for _, t := range m.tasks {
		cp := *t
		result = append(result, &cp)
	}
	return result
}

// CancelDispatcher provides a way to register and dispatch cancel signals
// for named operations (e.g., chat sessions, agent replies).
type CancelDispatcher struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// NewCancelDispatcher creates a new CancelDispatcher.
func NewCancelDispatcher() *CancelDispatcher {
	return &CancelDispatcher{
		cancels: make(map[string]context.CancelFunc),
	}
}

// Register creates a cancellable context for the given key.
func (d *CancelDispatcher) Register(key string) context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	if old, ok := d.cancels[key]; ok {
		old()
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.cancels[key] = cancel
	return ctx
}

// Cancel sends a cancel signal for the given key.
func (d *CancelDispatcher) Cancel(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cancel, ok := d.cancels[key]; ok {
		cancel()
		delete(d.cancels, key)
	}
}

// Unregister removes a key without canceling.
func (d *CancelDispatcher) Unregister(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.cancels, key)
}

// ChatRunInfo tracks an active chat/reply execution.
type ChatRunInfo struct {
	SessionID string
	AgentName string
	ReplyID   string
	StartedAt time.Time
}

// ChatRunRegistry tracks active chat/reply executions.
type ChatRunRegistry struct {
	mu   sync.RWMutex
	runs map[string]*ChatRunInfo
}

// NewChatRunRegistry creates a new registry.
func NewChatRunRegistry() *ChatRunRegistry {
	return &ChatRunRegistry{runs: make(map[string]*ChatRunInfo)}
}

// Register records a new active chat run.
func (r *ChatRunRegistry) Register(sessionID, agentName, replyID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[replyID] = &ChatRunInfo{
		SessionID: sessionID,
		AgentName: agentName,
		ReplyID:   replyID,
		StartedAt: time.Now(),
	}
}

// Unregister removes a completed chat run.
func (r *ChatRunRegistry) Unregister(replyID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.runs, replyID)
}

// Active returns all currently active runs.
func (r *ChatRunRegistry) Active() []*ChatRunInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*ChatRunInfo, 0, len(r.runs))
	for _, run := range r.runs {
		result = append(result, run)
	}
	return result
}

// IsActive returns true if the given reply is still running.
func (r *ChatRunRegistry) IsActive(replyID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.runs[replyID]
	return ok
}
