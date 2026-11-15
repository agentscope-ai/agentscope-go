package app

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/schedule"
	"github.com/sirupsen/logrus"
)

// SchedulerManager integrates the schedule package with the app layer.
// It creates scheduled tasks that trigger chat runs.
//
// Locking: m.mu guards the m.records map itself and nothing else. Every field
// of a published record is guarded by its own scheduleEntry.mu, because the
// scheduler invokes the task callback on another goroutine and that callback
// re-arms the chain (writing TaskID/Status) while HTTP handlers read and
// Cancel writes. Get/List hand out copies, never the live record.
type SchedulerManager struct {
	mu        sync.RWMutex
	scheduler schedule.Scheduler
	chatSvc   *ChatService
	records   map[string]*scheduleEntry

	// chatFn is a test seam: when set it replaces chatSvc.Chat for scheduled
	// runs. It is an atomic pointer because the callback goroutine reads it
	// while tests (or a future runtime reconfigure) may write it.
	chatFn atomic.Pointer[chatRunner]
}

// chatRunner is the signature of the scheduled-run seam.
type chatRunner func(ctx context.Context, sessionID, input string) error

// setChatFn installs (or, with nil, clears) the scheduled-run seam.
func (m *SchedulerManager) setChatFn(fn chatRunner) {
	if fn == nil {
		m.chatFn.Store(nil)
		return
	}
	m.chatFn.Store(&fn)
}

// runChat executes a scheduled chat run through the configured service.
func (m *SchedulerManager) runChat(ctx context.Context, sessionID, input string) error {
	if fn := m.chatFn.Load(); fn != nil && *fn != nil {
		return (*fn)(ctx, sessionID, input)
	}
	if m.chatSvc == nil {
		return fmt.Errorf("scheduler: no chat service configured")
	}
	_, err := m.chatSvc.Chat(ctx, sessionID, input)
	return err
}

// ScheduleRecord tracks a managed schedule.
type ScheduleRecord struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	CronExpr  string    `json:"cron_expr,omitempty"`
	Input     string    `json:"input"`
	TaskID    string    `json:"task_id"` // from scheduler
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// scheduleEntry is the internal, mutable half of a record. rec is stored by
// value so no live pointer can escape to a caller.
type scheduleEntry struct {
	mu sync.Mutex
	// canceled is sticky: once Cancel lands, the re-arm chain must stop even
	// if a run was already in flight and had passed its liveness check.
	canceled bool
	// armSeq numbers re-arm attempts. A scheduler may run the callback
	// synchronously inside Schedule, which nests re-arms: the commits then
	// unwind from the innermost outwards, and without a sequence number the
	// OLDEST task id would win and Cancel would target a task that already
	// fired. Only the commit matching the latest attempt is applied.
	armSeq int
	rec    ScheduleRecord
}

// snapshot copies the record under the entry lock.
func (e *scheduleEntry) snapshot() ScheduleRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rec
}

// NewSchedulerManager creates a scheduler manager.
func NewSchedulerManager(scheduler schedule.Scheduler, chatSvc *ChatService) *SchedulerManager {
	return &SchedulerManager{
		scheduler: scheduler,
		chatSvc:   chatSvc,
		records:   make(map[string]*scheduleEntry),
	}
}

// Create creates a new scheduled task.
func (m *SchedulerManager) Create(ctx context.Context, req CreateScheduleRequest) (*ScheduleRecord, error) {
	id := agentscope.GenerateID()
	entry := &scheduleEntry{rec: ScheduleRecord{
		ID:        id,
		SessionID: req.SessionID,
		CronExpr:  req.CronExpr,
		Input:     req.Input,
		Status:    "active",
		CreatedAt: time.Now(),
	}}

	task := &schedule.Task{
		Name:  fmt.Sprintf("schedule_%s", id),
		Input: req.Input,
	}

	// Upstream #2442: validate the cron expression BEFORE persisting. The
	// old parser silently folded unknown expressions (including standard
	// five-field cron) into an hourly interval; a bad schedule must fail
	// creation with a *CronValidationError instead.
	var cron *cronSchedule
	if req.CronExpr != "" {
		parsed, err := parseCronSchedule(req.CronExpr)
		if err != nil {
			return nil, err
		}
		if _, ok := parsed.next(time.Now()); !ok {
			return nil, &CronValidationError{
				Expr:   req.CronExpr,
				Reason: fmt.Sprintf("expression never fires within the next %d years", cronScanYears),
			}
		}
		// RunOnce asks for a single fire at the next matching slot. The
		// expression is still validated, since a malformed one is a client error
		// whether or not it repeats, but cron stays nil so the callback does not
		// re-arm. CreateScheduleRequest.RunOnce was previously accepted by the
		// API and read by nothing.
		if !req.RunOnce {
			cron = parsed
		}
		task.RunAt = mustNextFire(parsed, time.Now())
	} else {
		task.RunAt = time.Now().Add(time.Second)
	}

	// Publish the record BEFORE handing the task to the scheduler. A
	// scheduler is free to run the callback synchronously (or fire
	// immediately when RunAt is only microseconds ahead), and the callback
	// looks the record up to decide whether to re-arm. Publishing afterwards
	// made that first fire see no record and silently degrade a cron
	// schedule to a one-shot.
	m.mu.Lock()
	m.records[id] = entry
	m.mu.Unlock()

	sessionID := req.SessionID
	var fn schedule.TaskFunc
	fn = schedule.TaskFunc(func(ctx context.Context, t *schedule.Task) error {
		// A cancel that landed before this fire started must stop the run,
		// not just the re-arm.
		entry.mu.Lock()
		canceled := entry.canceled
		entry.mu.Unlock()
		if canceled {
			m.forgetTask(t.ID)
			return nil
		}

		chatErr := m.runChat(ctx, sessionID, t.Input)
		if chatErr != nil {
			logrus.WithError(chatErr).WithField("schedule", id).
				Warn("scheduled chat failed")
		}
		if cron == nil {
			// A RunOnce schedule (or one with no cron expression) fires once and
			// never re-arms. After that fire, mark the record terminal so Get and
			// List stop reporting a spent one-shot as "active", and drop the dead
			// task. The active-and-not-canceled gate matches the re-arm path below:
			// a cancel is sticky and wins, and a status the caller already moved off
			// "active" is left alone.
			entry.mu.Lock()
			if !entry.canceled && entry.rec.Status == "active" {
				entry.rec.Status = "completed"
			}
			entry.mu.Unlock()
			m.forgetTask(t.ID)
			return chatErr
		}

		entry.mu.Lock()
		active := !entry.canceled && entry.rec.Status == "active"
		var seq int
		if active {
			entry.armSeq++
			seq = entry.armSeq
		}
		entry.mu.Unlock()
		if !active {
			return chatErr
		}

		// Advance the cron grid from the slot that just fired so runs do not
		// drift. If that slot is already in the past (the run executed late,
		// e.g. after scheduler backlog or clock skew), skip past-due fires
		// instead of bursting and recompute from now.
		next, ok := cron.next(t.RunAt)
		if ok && !next.After(time.Now()) {
			next, ok = cron.next(time.Now())
		}
		if !ok {
			entry.mu.Lock()
			entry.rec.Status = "completed"
			entry.mu.Unlock()
			m.forgetTask(t.ID)
			return chatErr
		}

		nextTask := &schedule.Task{Name: t.Name, Input: t.Input, RunAt: next}
		nextID, schedErr := m.scheduler.Schedule(context.Background(), nextTask, fn)
		if schedErr != nil {
			logrus.WithError(schedErr).WithField("schedule", id).
				Error("failed to re-arm cron schedule")
			entry.mu.Lock()
			entry.rec.Status = "failed"
			entry.mu.Unlock()
			return schedErr
		}

		// Commit the new task ID under the same lock Cancel uses. If Cancel
		// landed while Schedule was in flight it already read the OLD task
		// ID, so the freshly armed task would survive and run one more chat
		// after the user canceled — detect that here and cancel it too.
		entry.mu.Lock()
		racedCancel := entry.canceled
		if !racedCancel && seq == entry.armSeq {
			entry.rec.TaskID = nextID
		}
		entry.mu.Unlock()
		if racedCancel {
			if err := m.scheduler.Cancel(context.Background(), nextID); err != nil {
				logrus.WithError(err).WithField("schedule", id).
					Warn("failed to cancel cron task armed during cancel race")
			}
		}

		// The task that just fired is dead weight: a cron chain schedules a
		// fresh one-shot per fire, so without removal a per-minute schedule
		// accumulates one entry (and one context.CancelFunc) per fire for
		// the lifetime of the process.
		m.forgetTask(t.ID)
		return chatErr
	})

	taskID, err := m.scheduler.Schedule(ctx, task, fn)
	if err != nil {
		m.mu.Lock()
		delete(m.records, id)
		m.mu.Unlock()
		return nil, fmt.Errorf("schedule task: %w", err)
	}

	// Backfill the task ID, but never overwrite one the callback already
	// committed: with a synchronous scheduler a whole re-arm cycle can
	// complete before Schedule returns.
	entry.mu.Lock()
	if entry.rec.TaskID == "" {
		entry.rec.TaskID = taskID
	}
	snap := entry.rec
	entry.mu.Unlock()
	return &snap, nil
}

// mustNextFire returns the next fire time for an expression already verified
// to fire; the fallback keeps the function total if the clock moves between
// validation and scheduling.
func mustNextFire(c *cronSchedule, from time.Time) time.Time {
	if next, ok := c.next(from); ok {
		return next
	}
	return from.Add(time.Hour)
}

// forgetTask drops an already-fired task from the scheduler when the backend
// supports it. Optional so custom Scheduler implementations are unaffected.
func (m *SchedulerManager) forgetTask(taskID string) {
	if taskID == "" {
		return
	}
	if r, ok := m.scheduler.(schedule.TaskRemover); ok {
		r.Remove(taskID)
	}
}

// Cancel cancels a scheduled task.
func (m *SchedulerManager) Cancel(ctx context.Context, id string) error {
	m.mu.RLock()
	entry, ok := m.records[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("schedule %s not found", id)
	}

	entry.mu.Lock()
	entry.canceled = true
	entry.rec.Status = "canceled"
	taskID := entry.rec.TaskID
	entry.mu.Unlock()

	if taskID == "" {
		// Never armed (Schedule is still in flight) or already dropped after
		// its final fire. The canceled flag above stops the chain either way.
		return nil
	}
	if err := m.scheduler.Cancel(ctx, taskID); err != nil {
		// The task may have fired and been removed between the record read
		// and this call. The canceled flag still guarantees no further runs,
		// so this is not a user-visible failure.
		logrus.WithError(err).WithField("schedule", id).
			Debug("scheduler cancel reported an error; record already marked canceled")
	}
	return nil
}

// Update applies mutate to a schedule record under the entry lock and returns
// the updated copy.
//
// Get and List hand out copies, so a handler that patched the returned pointer
// wrote to a throwaway value and answered 200 with a body that never reached
// the store. Status is not cosmetic: the re-arm callback checks it, so
// PATCH {"status":"paused"} is how a caller stops a chain without canceling it.
//
// Two status transitions are refused, because both would advertise a schedule
// that can never fire again:
//
//   - anything -> "active" once the chain has stopped. Pausing (or completing,
//     or failing) ends the re-arm chain: there is no armed task left to run.
//     Resuming would mean computing the next fire and arming a fresh task, and
//     doing that from inside the entry lock is unsafe (a scheduler may run the
//     callback synchronously, and the callback takes the same lock). Callers
//     create a new schedule instead.
//   - a canceled schedule, whose sticky canceled flag already tore the chain
//     down.
//
// mutate must not call back into the manager for the same id: it runs under the
// entry lock, so Get/List/Update on that id would deadlock. Patching a different
// id is safe (the map lock is not held).
func (m *SchedulerManager) Update(id string, mutate func(*ScheduleRecord)) (*ScheduleRecord, bool) {
	m.mu.RLock()
	entry, ok := m.records[id]
	m.mu.RUnlock()
	if !ok {
		return nil, false
	}

	entry.mu.Lock()
	prevStatus := entry.rec.Status
	if mutate != nil {
		mutate(&entry.rec)
	}
	switch {
	case entry.canceled:
		entry.rec.Status = "canceled"
	case entry.rec.Status == "active" && prevStatus != "active":
		// Refuse the resume rather than record a status the chain cannot
		// honor. See the doc comment. Only a record that is active RIGHT NOW
		// has an armed task behind it, so the test is against "active"
		// specifically and not against "some non-empty terminal status":
		// PATCH {"status":""} stops the chain too (the callback requires
		// "active"), and a second PATCH {"status":"active"} must not be let
		// through just because the status it is coming back from is empty.
		entry.rec.Status = prevStatus
	}
	snap := entry.rec
	entry.mu.Unlock()
	return &snap, true
}

// Get returns a copy of a schedule record by ID.
func (m *SchedulerManager) Get(id string) (*ScheduleRecord, bool) {
	m.mu.RLock()
	entry, ok := m.records[id]
	m.mu.RUnlock()
	if !ok {
		return nil, false
	}
	snap := entry.snapshot()
	return &snap, true
}

// List returns copies of all schedule records.
func (m *SchedulerManager) List() []*ScheduleRecord {
	m.mu.RLock()
	entries := make([]*scheduleEntry, 0, len(m.records))
	for _, e := range m.records {
		entries = append(entries, e)
	}
	m.mu.RUnlock()

	result := make([]*ScheduleRecord, 0, len(entries))
	for _, e := range entries {
		snap := e.snapshot()
		result = append(result, &snap)
	}
	return result
}
