package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/schedule"
)

// syncScheduler runs the task function INSIDE Schedule, which a real scheduler
// is allowed to do (and InMemoryScheduler effectively does when RunAt is only
// microseconds away and the computed delay is non-positive). The record must
// already be published at that point, otherwise the first fire cannot find it
// and the cron chain silently degrades to a one-shot.
type syncScheduler struct {
	mu       sync.Mutex
	tasks    []*schedule.Task
	fns      []schedule.TaskFunc
	ids      []string
	n        int
	depth    int
	maxDepth int
}

func (s *syncScheduler) Schedule(ctx context.Context, task *schedule.Task, fn schedule.TaskFunc) (string, error) {
	s.mu.Lock()
	s.n++
	id := fmt.Sprintf("task-%d", s.n)
	task.ID = id
	s.tasks = append(s.tasks, task)
	s.fns = append(s.fns, fn)
	s.ids = append(s.ids, id)
	depth := s.depth
	s.depth++
	s.mu.Unlock()

	if depth < s.maxDepth {
		_ = fn(ctx, task)
	}

	s.mu.Lock()
	s.depth--
	s.mu.Unlock()
	return id, nil
}

func (s *syncScheduler) Cancel(context.Context, string) error                { return nil }
func (s *syncScheduler) Get(context.Context, string) (*schedule.Task, error) { return nil, nil }
func (s *syncScheduler) List(context.Context) ([]*schedule.Task, error)      { return nil, nil }
func (s *syncScheduler) Close() error                                        { return nil }

func (s *syncScheduler) snapshot() (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tasks), s.ids[len(s.ids)-1]
}

// recordingScheduler is a concurrency-safe fake that captures task functions so
// a test can fire them by hand. When release is non-nil, every Schedule after
// the first blocks until release is closed, which lets a test park the re-arm
// at a precise point.
type recordingScheduler struct {
	mu       sync.Mutex
	n        int
	ids      []string
	tasks    []*schedule.Task
	fns      []schedule.TaskFunc
	canceled []string
	removed  []string
	entered  chan struct{}
	release  chan struct{}
}

func (s *recordingScheduler) Schedule(_ context.Context, task *schedule.Task, fn schedule.TaskFunc) (string, error) {
	s.mu.Lock()
	s.n++
	id := fmt.Sprintf("task-%d", s.n)
	task.ID = id
	s.ids = append(s.ids, id)
	s.tasks = append(s.tasks, task)
	s.fns = append(s.fns, fn)
	gate := s.release != nil && s.n > 1
	entered, release := s.entered, s.release
	s.mu.Unlock()

	if gate {
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		<-release
	}
	return id, nil
}

func (s *recordingScheduler) Cancel(_ context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canceled = append(s.canceled, taskID)
	return nil
}

func (s *recordingScheduler) Remove(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed = append(s.removed, taskID)
}

func (s *recordingScheduler) Get(context.Context, string) (*schedule.Task, error) { return nil, nil }
func (s *recordingScheduler) List(context.Context) ([]*schedule.Task, error)      { return nil, nil }
func (s *recordingScheduler) Close() error                                        { return nil }

func (s *recordingScheduler) fire(i int, task *schedule.Task) error {
	s.mu.Lock()
	fn := s.fns[i]
	s.mu.Unlock()
	return fn(context.Background(), task)
}

func (s *recordingScheduler) canceledIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.canceled))
	copy(out, s.canceled)
	return out
}

func (s *recordingScheduler) removedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.removed))
	copy(out, s.removed)
	return out
}

func (s *recordingScheduler) lastID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ids[len(s.ids)-1]
}

func (s *recordingScheduler) taskCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tasks)
}

func TestSchedulerCreateSurvivesSynchronousFirstFire(t *testing.T) {
	ss := &syncScheduler{maxDepth: 3}
	m := NewSchedulerManager(ss, nil)
	var runsMu sync.Mutex
	runs := 0
	m.setChatFn(func(_ context.Context, _ string, _ string) error {
		runsMu.Lock()
		runs++
		runsMu.Unlock()
		return nil
	})

	rec, err := m.Create(context.Background(), CreateScheduleRequest{
		SessionID: "s1", CronExpr: "* * * * *", Input: "hi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	runsMu.Lock()
	gotRuns := runs
	runsMu.Unlock()
	if gotRuns == 0 {
		t.Fatal("the scheduler never ran the task function")
	}

	armed, lastID := ss.snapshot()
	if armed < 2 {
		t.Fatalf("the first fire ran before the record was published, so the cron "+
			"never re-armed (one-shot degradation): %d task(s) armed", armed)
	}

	// Create must not clobber the TaskID the callback already committed.
	updated, ok := m.Get(rec.ID)
	if !ok {
		t.Fatal("record vanished")
	}
	if updated.TaskID != lastID {
		t.Errorf("record TaskID = %q, want the most recent %q", updated.TaskID, lastID)
	}
	if updated.Status != "active" {
		t.Errorf("status = %q, want active", updated.Status)
	}
}

// A Cancel that lands while the re-arm is in flight must still stop the chain.
// The old implementation read record.TaskID outside the lock and only ever
// canceled the task it had already seen, so the freshly armed one survived and
// ran one more chat after the user canceled.
func TestSchedulerCancelWinsRaceWithRearm(t *testing.T) {
	rs := &recordingScheduler{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	m := NewSchedulerManager(rs, nil)
	m.setChatFn(func(_ context.Context, _ string, _ string) error { return nil })

	rec, err := m.Create(context.Background(), CreateScheduleRequest{
		SessionID: "s1", CronExpr: "*/10 * * * *", Input: "hi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Fire the first task; its re-arm parks inside Schedule.
	var fnWG sync.WaitGroup
	fnWG.Add(1)
	go func() {
		defer fnWG.Done()
		rs.mu.Lock()
		task := rs.tasks[0]
		rs.mu.Unlock()
		_ = rs.fire(0, task)
	}()

	select {
	case <-rs.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-arm never reached Schedule")
	}

	if err := m.Cancel(context.Background(), rec.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	close(rs.release)
	fnWG.Wait()

	canceled := rs.canceledIDs()
	rearmed := rs.lastID()
	found := false
	for _, id := range canceled {
		if id == rearmed {
			found = true
		}
	}
	if !found {
		t.Errorf("re-armed task %q survived Cancel (canceled: %v)", rearmed, canceled)
	}

	updated, _ := m.Get(rec.ID)
	if updated.Status != "canceled" {
		t.Errorf("status = %q, want canceled", updated.Status)
	}
}

// A cancel that arrives after the re-arm committed must cancel the NEW task id,
// not the one that already fired.
func TestSchedulerCancelAfterRearmTargetsNewTask(t *testing.T) {
	rs := &recordingScheduler{}
	m := NewSchedulerManager(rs, nil)
	m.setChatFn(func(_ context.Context, _ string, _ string) error { return nil })

	rec, err := m.Create(context.Background(), CreateScheduleRequest{
		SessionID: "s1", CronExpr: "*/10 * * * *", Input: "hi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.mu.Lock()
	first := rs.tasks[0]
	rs.mu.Unlock()
	if err := rs.fire(0, first); err != nil {
		t.Fatalf("fire: %v", err)
	}

	if err := m.Cancel(context.Background(), rec.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	canceled := rs.canceledIDs()
	rearmed := rs.lastID()
	if len(canceled) != 1 || canceled[0] != rearmed {
		t.Errorf("canceled = %v, want exactly the re-armed %q", canceled, rearmed)
	}
}

// Get/List must hand out copies: HTTP handlers json.Marshal them outside any
// lock while the scheduler callback rewrites TaskID and Status.
func TestSchedulerGetListReturnCopies(t *testing.T) {
	rs := &recordingScheduler{}
	m := NewSchedulerManager(rs, nil)
	m.setChatFn(func(_ context.Context, _ string, _ string) error { return nil })
	rec, err := m.Create(context.Background(), CreateScheduleRequest{
		SessionID: "s1", CronExpr: "*/10 * * * *", Input: "hi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, ok := m.Get(rec.ID)
	if !ok {
		t.Fatal("record not found")
	}
	got.Status = "mutated-by-caller"
	got.TaskID = "mutated"

	again, _ := m.Get(rec.ID)
	if again.Status != "active" {
		t.Errorf("status = %q; Get handed out the live record", again.Status)
	}
	if again.TaskID == "mutated" {
		t.Error("TaskID was mutated through the returned pointer")
	}

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("list len = %d", len(list))
	}
	list[0].Status = "also-mutated"
	if third, _ := m.Get(rec.ID); third.Status != "active" {
		t.Errorf("status = %q; List handed out the live record", third.Status)
	}
}

// Concurrent fires, cancels and reads must be race-free. Run with -race.
func TestSchedulerConcurrentCancelAndRearmIsRaceFree(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		rs := &recordingScheduler{}
		m := NewSchedulerManager(rs, nil)
		m.setChatFn(func(_ context.Context, _ string, _ string) error { return nil })
		rec, err := m.Create(context.Background(), CreateScheduleRequest{
			SessionID: "s1", CronExpr: "* * * * *", Input: "hi",
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}

		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = rs.fire(0, &schedule.Task{ID: "fired", Input: "hi", RunAt: time.Now()})
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Cancel(context.Background(), rec.ID)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if r, ok := m.Get(rec.ID); ok {
					_ = r.Status + r.TaskID
				}
				for _, r := range m.List() {
					_ = r.Status + r.TaskID
				}
			}
		}()
		wg.Wait()

		if _, ok := m.Get(rec.ID); !ok {
			t.Fatal("record vanished")
		}
	}
}

// A cron chain schedules a fresh one-shot task per fire. Without dropping the
// task that just ran, a per-minute schedule accumulates one dead scheduler
// entry (and one context.CancelFunc) per fire for the life of the process.
func TestSchedulerForgetsFiredTaskWhenSupported(t *testing.T) {
	rs := &recordingScheduler{}
	m := NewSchedulerManager(rs, nil)
	m.setChatFn(func(_ context.Context, _ string, _ string) error { return nil })
	_, err := m.Create(context.Background(), CreateScheduleRequest{
		SessionID: "s1", CronExpr: "*/10 * * * *", Input: "hi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rs.mu.Lock()
	first := rs.tasks[0]
	rs.mu.Unlock()
	_ = rs.fire(0, first)

	found := false
	for _, id := range rs.removedIDs() {
		if id == first.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("fired task %q was not removed (removed = %v)", first.ID, rs.removedIDs())
	}
	// The chain must still have armed its replacement.
	if rs.taskCount() != 2 {
		t.Errorf("tasks armed = %d, want 2 (the original plus the re-arm)", rs.taskCount())
	}
}

// A scheduler without the optional TaskRemover capability must still work.
func TestSchedulerForgetCallsWithoutRemoverAreNoop(t *testing.T) {
	fs := &fakeScheduler{}
	m := NewSchedulerManager(fs, nil)
	m.setChatFn(func(_ context.Context, _ string, _ string) error { return nil })
	rec, err := m.Create(context.Background(), CreateScheduleRequest{
		SessionID: "s1", CronExpr: "*/10 * * * *", Input: "hi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := fs.fns[0](context.Background(), fs.tasks[0]); err != nil {
		t.Fatalf("task fn: %v", err)
	}
	if len(fs.tasks) != 2 {
		t.Errorf("expected a re-arm, got %d tasks", len(fs.tasks))
	}
	if _, ok := m.Get(rec.ID); !ok {
		t.Error("record vanished")
	}
}

// The real scheduler must implement the optional removal capability, and
// Remove must release the entry so it cannot be listed afterwards.
func TestInMemorySchedulerRemove(t *testing.T) {
	s := schedule.NewInMemoryScheduler()
	defer s.Close()

	var remover schedule.TaskRemover = s
	done := make(chan struct{})
	id, err := s.Schedule(context.Background(),
		&schedule.Task{Name: "t", RunAt: time.Now().Add(-time.Second)},
		func(context.Context, *schedule.Task) error { close(done); return nil })
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	<-done

	remover.Remove(id)
	if _, err := s.Get(context.Background(), id); err == nil {
		t.Error("a removed task must no longer be retrievable")
	}
	// Removing an unknown ID must not panic.
	remover.Remove("does-not-exist")
}

// Canceling a schedule whose task already fired and was dropped must not
// surface a spurious "task not found" failure: the record is authoritative and
// the canceled flag stops the chain either way.
func TestSchedulerCancelAfterTaskForgottenIsNotAnError(t *testing.T) {
	rs := &recordingScheduler{}
	m := NewSchedulerManager(rs, nil)
	m.setChatFn(func(_ context.Context, _ string, _ string) error { return nil })
	rec, err := m.Create(context.Background(), CreateScheduleRequest{
		SessionID: "s1", CronExpr: "*/10 * * * *", Input: "hi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.mu.Lock()
	first := rs.tasks[0]
	rs.mu.Unlock()
	_ = rs.fire(0, first)

	// Force the record back onto the already-fired task id and make Cancel
	// fail, as a real scheduler does for a removed entry.
	rs.mu.Lock()
	rs.ids[len(rs.ids)-1] = first.ID
	rs.mu.Unlock()
	if err := m.Cancel(context.Background(), rec.ID); err != nil {
		t.Errorf("cancel after the task was forgotten: %v", err)
	}
	if updated, _ := m.Get(rec.ID); updated.Status != "canceled" {
		t.Errorf("status = %q, want canceled", updated.Status)
	}
}

// Get returns a copy, so a handler that patched the returned pointer wrote to a
// throwaway value and answered 200 with a body the store never saw. Update is
// the mutation path; Status is not cosmetic — the re-arm callback reads it, so
// pausing through Update has to actually stop the chain.
func TestSchedulerUpdatePersistsAndCanPauseChain(t *testing.T) {
	rs := &recordingScheduler{}
	m := NewSchedulerManager(rs, nil)
	m.setChatFn(func(_ context.Context, _ string, _ string) error { return nil })
	rec, err := m.Create(context.Background(), CreateScheduleRequest{
		SessionID: "s1", CronExpr: "*/10 * * * *", Input: "hi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Patching the copy from Get must not be enough.
	if stale, ok := m.Get(rec.ID); ok {
		stale.Input = "written-to-a-copy"
	}
	if cur, _ := m.Get(rec.ID); cur.Input != "hi" {
		t.Errorf("Input = %q; Get handed out the live record", cur.Input)
	}

	updated, ok := m.Update(rec.ID, func(r *ScheduleRecord) {
		r.Input = "new input"
		r.Status = "paused"
	})
	if !ok {
		t.Fatal("update reported not found")
	}
	if updated.Input != "new input" || updated.Status != "paused" {
		t.Errorf("returned record = %+v, want the patched values", updated)
	}
	persisted, _ := m.Get(rec.ID)
	if persisted.Input != "new input" {
		t.Errorf("persisted Input = %q, want %q", persisted.Input, "new input")
	}
	if persisted.Status != "paused" {
		t.Errorf("persisted Status = %q, want %q", persisted.Status, "paused")
	}

	// The re-arm callback must honor the paused status and not re-arm.
	rs.mu.Lock()
	first := rs.tasks[0]
	rs.mu.Unlock()
	if err := rs.fire(0, first); err != nil {
		t.Fatalf("fire: %v", err)
	}
	if rs.taskCount() != 1 {
		t.Errorf("a paused schedule re-armed: %d tasks", rs.taskCount())
	}
}

// A canceled schedule has already had its task chain torn down, so flipping the
// status back to active would advertise a schedule that can never fire again.
func TestSchedulerUpdateCannotResurrectCanceled(t *testing.T) {
	rs := &recordingScheduler{}
	m := NewSchedulerManager(rs, nil)
	m.setChatFn(func(_ context.Context, _ string, _ string) error { return nil })
	rec, err := m.Create(context.Background(), CreateScheduleRequest{
		SessionID: "s1", CronExpr: "*/10 * * * *", Input: "hi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.Cancel(context.Background(), rec.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	updated, ok := m.Update(rec.ID, func(r *ScheduleRecord) { r.Status = "active" })
	if !ok {
		t.Fatal("update reported not found")
	}
	if updated.Status != "canceled" {
		t.Errorf("Status = %q; a canceled schedule must not be resurrected", updated.Status)
	}

	rs.mu.Lock()
	first := rs.tasks[0]
	rs.mu.Unlock()
	if err := rs.fire(0, first); err != nil {
		t.Fatalf("fire: %v", err)
	}
	if rs.taskCount() != 1 {
		t.Errorf("a canceled schedule re-armed: %d tasks", rs.taskCount())
	}
}

func TestSchedulerUpdateMissingID(t *testing.T) {
	m := NewSchedulerManager(&recordingScheduler{}, nil)
	if _, ok := m.Update("nope", func(*ScheduleRecord) {}); ok {
		t.Error("updating an unknown id must report not found")
	}
	// A nil mutate function is a read-through, not a panic.
	rs := &recordingScheduler{}
	m2 := NewSchedulerManager(rs, nil)
	m2.setChatFn(func(_ context.Context, _ string, _ string) error { return nil })
	rec, err := m2.Create(context.Background(), CreateScheduleRequest{
		SessionID: "s1", CronExpr: "*/10 * * * *", Input: "hi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, ok := m2.Update(rec.ID, nil)
	if !ok || got.Input != "hi" {
		t.Errorf("nil-mutate update = %+v, %v", got, ok)
	}
}

// The PATCH route must persist through Update, not through the copy Get
// returned. This is the HTTP-level regression test for that.
func TestPatchScheduleRoutePersists(t *testing.T) {
	rs := &recordingScheduler{}
	app := &App{schedulerMgr: NewSchedulerManager(rs, nil)}
	app.schedulerMgr.setChatFn(func(_ context.Context, _ string, _ string) error { return nil })
	rec, err := app.schedulerMgr.Create(context.Background(), CreateScheduleRequest{
		SessionID: "s1", CronExpr: "*/10 * * * *", Input: "hi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /api/schedule/{id}", app.handleUpdateSchedule)
	body := strings.NewReader(`{"input":"patched","status":"paused"}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/schedule/"+rec.ID, body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	persisted, ok := app.schedulerMgr.Get(rec.ID)
	if !ok {
		t.Fatal("record vanished")
	}
	if persisted.Input != "patched" {
		t.Errorf("persisted Input = %q, want %q (the PATCH was applied to a copy)", persisted.Input, "patched")
	}
	if persisted.Status != "paused" {
		t.Errorf("persisted Status = %q, want %q", persisted.Status, "paused")
	}
	var echoed ScheduleRecord
	if err := json.Unmarshal(w.Body.Bytes(), &echoed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if echoed.Input != persisted.Input || echoed.Status != persisted.Status {
		t.Errorf("response body %+v disagrees with the store %+v", echoed, persisted)
	}
}

// Pausing ends the re-arm chain: there is no armed task left to run. Flipping
// the status back to "active" would therefore advertise a schedule that can
// never fire, so the transition is refused rather than recorded.
func TestSchedulerUpdateRefusesResumeOfStoppedChain(t *testing.T) {
	rs := &recordingScheduler{}
	m := NewSchedulerManager(rs, nil)
	m.setChatFn(func(_ context.Context, _ string, _ string) error { return nil })
	rec, err := m.Create(context.Background(), CreateScheduleRequest{
		SessionID: "s1", CronExpr: "*/10 * * * *", Input: "hi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, ok := m.Update(rec.ID, func(r *ScheduleRecord) { r.Status = "paused" }); !ok {
		t.Fatal("pause update reported not found")
	}
	if got, _ := m.Get(rec.ID); got.Status != "paused" {
		t.Fatalf("Status = %q, want paused", got.Status)
	}

	updated, ok := m.Update(rec.ID, func(r *ScheduleRecord) { r.Status = "active" })
	if !ok {
		t.Fatal("resume update reported not found")
	}
	if updated.Status == "active" {
		t.Error("a paused chain was marked active again; nothing is armed to fire")
	}
	if updated.Status != "paused" {
		t.Errorf("Status = %q, want it left at paused", updated.Status)
	}
	persisted, _ := m.Get(rec.ID)
	if persisted.Status != "paused" {
		t.Errorf("persisted Status = %q, want paused", persisted.Status)
	}

	// Other fields of the same patch must still be applied.
	both, _ := m.Update(rec.ID, func(r *ScheduleRecord) {
		r.Input = "changed"
		r.Status = "active"
	})
	if both.Input != "changed" {
		t.Errorf("Input = %q, want the non-status part of the patch applied", both.Input)
	}
	if both.Status != "paused" {
		t.Errorf("Status = %q, want paused", both.Status)
	}
}

// A terminal status set by the chain itself (completed / failed) must not be
// resurrectable either.
func TestSchedulerUpdateRefusesResumeOfTerminalStatus(t *testing.T) {
	rs := &recordingScheduler{}
	m := NewSchedulerManager(rs, nil)
	m.setChatFn(func(_ context.Context, _ string, _ string) error { return nil })
	rec, err := m.Create(context.Background(), CreateScheduleRequest{
		SessionID: "s1", CronExpr: "*/10 * * * *", Input: "hi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, terminal := range []string{"completed", "failed"} {
		m.Update(rec.ID, func(r *ScheduleRecord) { r.Status = terminal })
		updated, _ := m.Update(rec.ID, func(r *ScheduleRecord) { r.Status = "active" })
		if updated.Status != terminal {
			t.Errorf("after %q: Status = %q, want it left at %q", terminal, updated.Status, terminal)
		}
	}
}

// Two PATCHes must not open a resume path: blanking the status stops the chain
// (the callback requires "active"), and a following {"status":"active"} has to
// be refused too — the guard tests against "active" specifically, not against
// "some non-empty terminal status".
func TestSchedulerUpdateRefusesResumeAfterBlankStatus(t *testing.T) {
	rs := &recordingScheduler{}
	m := NewSchedulerManager(rs, nil)
	m.setChatFn(func(_ context.Context, _ string, _ string) error { return nil })
	rec, err := m.Create(context.Background(), CreateScheduleRequest{
		SessionID: "s1", CronExpr: "*/10 * * * *", Input: "hi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, ok := m.Update(rec.ID, func(r *ScheduleRecord) { r.Status = "" }); !ok {
		t.Fatal("first update reported not found")
	}
	// The chain is dead now: the callback only re-arms while status is active.
	rs.mu.Lock()
	first := rs.tasks[0]
	rs.mu.Unlock()
	if err := rs.fire(0, first); err != nil {
		t.Fatalf("fire: %v", err)
	}
	if rs.taskCount() != 1 {
		t.Fatalf("a blanked status still re-armed: %d tasks", rs.taskCount())
	}

	updated, _ := m.Update(rec.ID, func(r *ScheduleRecord) { r.Status = "active" })
	if updated.Status == "active" {
		t.Error("resume succeeded from a blank status; nothing is armed to fire")
	}
	if updated.Status != "" {
		t.Errorf("Status = %q, want it left blank", updated.Status)
	}
}

// A refused status transition must be reported, not answered with a 200 whose
// body silently disagrees with the request.
func TestPatchScheduleRouteReportsRefusedTransition(t *testing.T) {
	rs := &recordingScheduler{}
	app := &App{schedulerMgr: NewSchedulerManager(rs, nil)}
	app.schedulerMgr.setChatFn(func(_ context.Context, _ string, _ string) error { return nil })
	rec, err := app.schedulerMgr.Create(context.Background(), CreateScheduleRequest{
		SessionID: "s1", CronExpr: "*/10 * * * *", Input: "hi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /api/schedule/{id}", app.handleUpdateSchedule)

	patch := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/api/schedule/"+rec.ID, strings.NewReader(body))
		mux.ServeHTTP(w, req)
		return w
	}

	if w := patch(`{"status":"paused"}`); w.Code != http.StatusOK {
		t.Fatalf("pausing: status = %d, body = %s", w.Code, w.Body.String())
	}
	w := patch(`{"status":"active"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("resuming a stopped chain: status = %d, want 409 (body = %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cannot be resumed") {
		t.Errorf("409 body should explain the refusal, got %q", w.Body.String())
	}
	// The refused patch must not have changed anything else either.
	persisted, _ := app.schedulerMgr.Get(rec.ID)
	if persisted.Status != "paused" {
		t.Errorf("persisted Status = %q, want paused", persisted.Status)
	}

	// A patch that does not touch status stays a 200.
	if w := patch(`{"input":"still editable"}`); w.Code != http.StatusOK {
		t.Errorf("input-only patch: status = %d, want 200 (body = %s)", w.Code, w.Body.String())
	}
	if persisted, _ = app.schedulerMgr.Get(rec.ID); persisted.Input != "still editable" {
		t.Errorf("persisted Input = %q", persisted.Input)
	}
}
