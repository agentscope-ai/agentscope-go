package thread

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// coordHost is the Host handle of one participant. Every method checks that
// the caller named in the request is that participant: a participant cannot
// write or read as another.
type coordHost struct {
	c  *Coordinator
	ps *partState
}

var _ Host = (*coordHost)(nil)

func (h *coordHost) caller(name string) error {
	if name != h.ps.name {
		return fmt.Errorf("thread: participant %s may not act as %q", h.ps.name, name)
	}
	return nil
}

// target resolves a participant other than the caller.
func (h *coordHost) target(name string) (*partState, error) {
	ps := h.c.parts[name]
	switch {
	case ps == nil:
		return nil, fmt.Errorf("thread: unknown participant %q", name)
	case ps == h.ps:
		return nil, fmt.Errorf("thread: %s cannot address itself", name)
	}
	return ps, nil
}

// running returns the caller's running activation, nil if none.
func (h *coordHost) runningLocked() *activation { return h.ps.running }

// writableLocked is the check every write makes in the same step as its
// append: the Coordinator is open, and neither the caller's activation a
// (nil outside an activation) nor ctx was canceled.
func (h *coordHost) writableLocked(ctx context.Context, a *activation) error {
	if h.c.closed {
		return ErrClosed
	}
	if a != nil {
		if err := a.ctx.Err(); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrInterrupted, h.ps.name, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrInterrupted, h.ps.name, err)
	}
	return nil
}

// lockForWrite takes the mutex for a write and checks that it may be made
// (writableLocked). It returns with the mutex held, or with an error and the
// mutex released.
func (h *coordHost) lockForWrite(ctx context.Context) error {
	h.c.mu.Lock()
	if err := h.writableLocked(ctx, h.runningLocked()); err != nil {
		h.c.mu.Unlock()
		return err
	}
	return nil
}

// orBackground returns ctx, or a background context for a nil one, so a
// participant that passes none can still write.
func orBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// basedOnLocked is the trigger seq of the caller's running activation.
func (h *coordHost) basedOnLocked() uint64 {
	if a := h.runningLocked(); a != nil {
		return a.trigger.Seq
	}
	return 0
}

// checkTransferLocked returns the caller's activation when it holds the
// foreground at epoch.
func (h *coordHost) checkTransferLocked(epoch uint64) (*activation, error) {
	c := h.c
	if c.closed {
		return nil, ErrClosed
	}
	a := h.runningLocked()
	if a == nil || !c.holdsForegroundLocked(a) || c.epoch != epoch || a.handedTo != "" {
		return nil, fmt.Errorf("%w: %s does not hold the foreground at epoch %d (the owner is %q at epoch %d)",
			ErrNotOwner, h.ps.name, epoch, c.owner, c.epoch)
	}
	return a, nil
}

// Transfer hands the foreground to another participant. The caller's
// visible text is sealed as its reply in the same append as the owner
// change, under the owner and epoch guard. The target's foreground
// activation is queued; summary forks are not waited for.
func (h *coordHost) Transfer(ctx context.Context, req TransferRequest) (uint64, error) { //nolint:gocritic // the Host interface takes requests by value
	c := h.c
	ctx = orBackground(ctx)
	if err := h.caller(req.From.Participant); err != nil {
		return 0, err
	}
	to, err := h.target(req.To)
	if err != nil {
		return 0, err
	}
	if err := req.Brief.Validate(to.desc.BriefMaxRunes); err != nil {
		return 0, err
	}
	if !c.available(to.name) {
		return 0, fmt.Errorf("thread: %s is unavailable", to.name)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	a, err := h.checkTransferLocked(req.Epoch)
	if err != nil {
		return 0, err
	}
	if err := h.writableLocked(ctx, a); err != nil {
		return 0, err
	}
	t := a.trigger.Seq
	var batch []Entry
	steered := a.steeredSeqs()
	if strings.TrimSpace(req.Text) != "" || cloneDigest(req.Digest) != nil {
		batch = append(batch, Entry{
			Kind:     KindReply,
			Origin:   Origin{Participant: h.ps.name},
			BasedOn:  t,
			Content:  textContent(req.Text),
			Digest:   cloneDigest(req.Digest),
			Steered:  steered,
			DedupKey: "reply:" + a.id,
		})
		steered = nil
	}
	b := req.Brief
	batch = append(batch, Entry{
		Kind:     KindOwnerChange,
		Origin:   req.From,
		BasedOn:  t,
		Steered:  steered,
		Owner:    &OwnerChange{From: h.ps.name, To: to.name, Reason: ReasonTransfer, Brief: cloneBrief(&b)},
		DedupKey: "transfer:" + a.id,
	})
	stored, err := c.appendLocked(batch, &Guard{Owner: c.owner, Epoch: c.epoch})
	if err != nil {
		return 0, fmt.Errorf("thread: commit transfer: %w", err)
	}
	if len(stored) != len(batch) {
		return 0, errors.New("thread: transfer was already committed")
	}
	oc := &stored[len(stored)-1]
	a.handedTo = to.name
	a.eventsBefore = req.EventsBefore
	na := c.newActivationLocked(to, actTransfer, oc, true, 0)
	na.cycleInput = a.cycleInput
	if !a.streamDone && a.forwarded < req.EventsBefore {
		// Events shown before the Transfer are still unread: they reach the
		// user first, then the Transfer's entries, then the new owner.
		a.backlog = make(chan struct{})
		a.held = stored
		c.open = a
		na.pred = a
	} else {
		c.emitEntriesLocked(stored)
	}
	h.ps.lastSealed = oc.Seq
	c.startForegroundLocked(na)
	return oc.Seq, nil
}

// Assign starts a background task on another participant.
func (h *coordHost) Assign(ctx context.Context, req AssignRequest) (string, uint64, error) { //nolint:gocritic // the Host interface takes requests by value
	c := h.c
	ctx = orBackground(ctx)
	if err := h.caller(req.From.Participant); err != nil {
		return "", 0, err
	}
	to, err := h.target(req.To)
	if err != nil {
		return "", 0, err
	}
	if strings.TrimSpace(req.Text) == "" {
		return "", 0, errors.New("thread: a task needs text")
	}
	if !c.available(to.name) {
		return "", 0, fmt.Errorf("thread: %s is unavailable", to.name)
	}
	c.mu.Lock()
	depth := req.Depth
	if a := h.runningLocked(); a != nil {
		depth = a.depth
	}
	c.mu.Unlock()
	if depth >= c.cfg.Limits.MaxAssignDepth {
		return "", 0, fmt.Errorf("thread: task depth %d reached the limit of %d; do the work here or report back", depth, c.cfg.Limits.MaxAssignDepth)
	}

	if err := h.lockForWrite(ctx); err != nil {
		return "", 0, err
	}
	defer c.mu.Unlock()
	c.nextTask++
	id := fmt.Sprintf("t-%d", c.nextTask)
	stored := c.commitLocked(h.ps.name, []Entry{{
		Kind:     KindAssign,
		Origin:   req.From,
		To:       []string{to.name},
		BasedOn:  h.basedOnLocked(),
		Task:     &Task{ID: id, Notify: req.Notify},
		Content:  textContent(req.Text),
		Refs:     slices.Clone(req.Refs),
		DedupKey: "task:" + c.runID + ":" + id,
	}}, nil)
	if len(stored) != 1 {
		return "", 0, errors.New("thread: the task could not be recorded")
	}
	tk := &taskRec{seq: stored[0].Seq, id: id, assigner: h.ps.name, assignee: to.name, notify: req.Notify, depth: depth + 1}
	if a := h.runningLocked(); a != nil {
		tk.assignerAct = a.id
	}
	c.tasks[tk.seq] = tk
	c.queueTaskLocked(tk, &stored[0])
	return id, tk.seq, nil
}

// Report publishes a result to other participants. A Notify report wakes a
// recipient that is the idle owner; a recipient running in the foreground
// receives it at its next boundary through Pending; otherwise it wakes the
// recipient once that is the idle owner (a background activation receives
// only the reports of tasks it assigned itself).
func (h *coordHost) Report(ctx context.Context, req ReportRequest) (uint64, error) { //nolint:gocritic // the Host interface takes requests by value
	c := h.c
	ctx = orBackground(ctx)
	if err := h.caller(req.From.Participant); err != nil {
		return 0, err
	}
	if strings.TrimSpace(req.Text) == "" && len(req.Refs) == 0 {
		return 0, errors.New("thread: a report needs text or artifacts")
	}
	var to []string
	var recipients []*partState
	for _, name := range req.To {
		if slices.Contains(to, name) {
			continue
		}
		ps, err := h.target(name)
		if err != nil {
			return 0, err
		}
		to = append(to, name)
		recipients = append(recipients, ps)
	}
	if err := h.lockForWrite(ctx); err != nil {
		return 0, err
	}
	defer c.mu.Unlock()
	e := Entry{
		Kind:    KindReport,
		Origin:  req.From,
		To:      to,
		BasedOn: h.basedOnLocked(),
		Notify:  req.Notify,
		Content: textContent(req.Text),
		Refs:    slices.Clone(req.Refs),
	}
	if req.TaskID != "" {
		e.Task = &Task{ID: req.TaskID, Notify: req.Notify}
	}
	stored := c.commitLocked(h.ps.name, []Entry{e}, nil)
	if len(stored) != 1 {
		return 0, errors.New("thread: the report could not be recorded")
	}
	if req.Notify {
		for _, r := range recipients {
			c.notifyLocked(r, &stored[0])
		}
	}
	return stored[0].Seq, nil
}

// Publish records an artifact.
func (h *coordHost) Publish(ctx context.Context, req PublishRequest) (uint64, error) { //nolint:gocritic // the Host interface takes requests by value
	c := h.c
	ctx = orBackground(ctx)
	if err := h.caller(req.From.Participant); err != nil {
		return 0, err
	}
	if len(req.Refs) == 0 {
		return 0, errors.New("thread: publish needs at least one artifact reference")
	}
	if err := h.lockForWrite(ctx); err != nil {
		return 0, err
	}
	defer c.mu.Unlock()
	stored := c.commitLocked(h.ps.name, []Entry{{
		Kind:    KindArtifact,
		Origin:  req.From,
		BasedOn: h.basedOnLocked(),
		Refs:    slices.Clone(req.Refs),
		Content: textContent(req.Text),
	}}, nil)
	if len(stored) != 1 {
		return 0, errors.New("thread: the artifact could not be recorded")
	}
	return stored[0].Seq, nil
}

// Read returns entries of the log. With Seqs it returns exactly those
// entries, leaving out unknown ones without saying which.
func (h *coordHost) Read(ctx context.Context, reader string, q ReadQuery) ([]Entry, error) {
	if err := h.caller(reader); err != nil {
		return nil, err
	}
	if len(q.Seqs) == 0 {
		return h.c.store.Read(ctx, q.After, Filter{Kinds: q.Kinds}, q.Limit)
	}
	var out []Entry
	for _, seq := range q.Seqs {
		e, ok, err := h.c.store.Get(ctx, seq)
		if err != nil {
			return nil, err
		}
		if !ok || (len(q.Kinds) > 0 && !slices.Contains(q.Kinds, e.Kind)) {
			continue
		}
		out = append(out, e)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

// EventOutline lists the event records the reader may read.
func (h *coordHost) EventOutline(reader string, limit int) []EventSummary {
	if h.caller(reader) != nil {
		return nil
	}
	return h.c.events.Outline(h.ps.desc.Zone, limit)
}

// EventSearch searches the event records the reader may read.
func (h *coordHost) EventSearch(reader, query string, limit int) []EventSummary {
	if h.caller(reader) != nil {
		return nil
	}
	return h.c.events.Search(h.ps.desc.Zone, query, limit)
}

// EventPage reads one page of an event record the reader may read.
func (h *coordHost) EventPage(reader, id string, offset, limit int) (string, int, error) {
	if err := h.caller(reader); err != nil {
		return "", 0, err
	}
	return h.c.events.Page(h.ps.desc.Zone, id, offset, limit)
}

// RecordEvent stores a tool output under the caller's name and zone.
func (h *coordHost) RecordEvent(r EventRecord) string { //nolint:gocritic // the Host interface takes records by value
	r.Origin.Participant = h.ps.name
	r.Zone = h.ps.desc.Zone
	return h.c.events.Add(r)
}

// RecordUsage records a model call under the caller's name.
func (h *coordHost) RecordUsage(e LedgerEntry) { //nolint:gocritic // the Host interface takes entries by value
	e.Participant = h.ps.name
	if e.Time.IsZero() {
		e.Time = h.c.cfg.Clock()
	}
	h.c.ledger.Record(e)
}

// Pending returns the entries for the running participant past cursor, as a
// Sync, or nil.
func (h *coordHost) Pending(ctx context.Context, participant string, cursor uint64) (*Sync, error) {
	if err := h.caller(participant); err != nil {
		return nil, err
	}
	return h.c.pending(ctx, h.ps, cursor)
}

// Descriptors returns every participant's descriptor in configuration order.
func (h *coordHost) Descriptors() []Descriptor {
	out := make([]Descriptor, 0, len(h.c.order))
	for _, ps := range h.c.order {
		out = append(out, cloneDescriptor(&ps.desc))
	}
	return out
}

// Owner returns the current owner and epoch.
func (h *coordHost) Owner() (string, uint64) { return h.c.Owner() }
