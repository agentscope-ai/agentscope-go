package thread

import (
	"context"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// importResult is what prepareImport hands an activation.
type importResult struct {
	sync   *Sync
	budget int // import budget in tokens, 0 when unlimited
	tokens int // tokens of sync's messages
	// clear is set when sync carries a clear's reset: the state to record
	// once the activation's Sync is known to be applied.
	clear *clearCommit
}

// clearCommit is what a clear changes in the Coordinator's view of the
// participant once its reset is applied.
type clearCommit struct {
	seq      uint64    // the summary's seq
	coverage *Coverage // the summary's coverage
}

// importer imports the entries before an activation's trigger into its
// participant. It calls the participant (Sync, Compact, State) and must run
// without the mutex.
type importer struct {
	c       *Coordinator
	ps      *partState
	ctx     context.Context
	entries []Entry // seqs 1..trigger-1, entries[i].Seq == i+1
	trigger uint64
	// restored is set for the participant's first import after a restart.
	restored bool
}

// prepareImport builds the Sync applied before the activation's reply. The
// trigger itself is never in it: it reaches the participant as the input.
// A trigger at or behind the participant's cursor (a wake for a report a
// background activation already imported) imports nothing.
//
// An edge participant is cleared when it returns after another participant
// held the foreground (the clear rule is for a takeover) and the newest
// summary written for it covers entries past its own last activity: one
// Sync resets its context to that summary and the projection of the entries
// it does not cover. Otherwise the entries between its cursor and the trigger are
// projected. Only an edge import has a budget; one that does not fit is
// the local fallback: it is applied in chunks with a compaction after each,
// and when compaction stops helping the rest is skipped with a message
// naming the range for thread_read.
func (c *Coordinator) prepareImport(ctx context.Context, a *activation, st ParticipantState) (importResult, error) {
	ps := a.part
	t := a.trigger.Seq
	if t <= 1 || t <= st.Cursor {
		im := &importer{c: c, ps: ps}
		return importResult{budget: im.budgetFor(st)}, nil
	}
	entries, err := c.store.Read(ctx, 0, Filter{}, int(t-1))
	if err != nil {
		return importResult{}, fmt.Errorf("thread: read entries for %s: %w", ps.name, err)
	}
	c.mu.Lock()
	lastAct, held, resetFrom, restored := ps.lastActivity, cloneCoverage(ps.heldCoverage), ps.resetFrom, ps.restored
	c.mu.Unlock()

	im := &importer{c: c, ps: ps, ctx: ctx, entries: entries, trigger: t, restored: restored}
	if ps.desc.Zone == ZoneEdge {
		if s := newestSummaryFor(entries, ps.name); s != nil && s.Coverage.Through > lastAct &&
			s.Seq != resetFrom && foregroundLeft(entries, ps.name, lastAct) {
			return im.clear(s), nil
		}
	}
	return im.incremental(st, held)
}

// foregroundLeft reports whether a participant other than name held the
// foreground after seq: the owner in force at seq, or the target of a later
// owner change, is another participant. A participant that kept the
// foreground since its last activity is not taken over, whatever summaries
// a background task of another participant produced meanwhile.
func foregroundLeft(entries []Entry, name string, seq uint64) bool {
	owner := ""
	for i := range entries {
		e := &entries[i]
		if e.Kind != KindOwnerChange || e.Owner == nil {
			continue
		}
		if e.Seq <= seq {
			owner = e.Owner.To
			continue
		}
		if e.Owner.To != name {
			return true
		}
	}
	return owner != "" && owner != name
}

// clear builds the reset import from summary s as one Sync: the context is
// emptied, s becomes the peer summary, and every entry s does not cover is
// projected. The participant's own entries are projected too when s does
// not cover them, because the reset drops them from its context. The
// activation has no budget, neither for this Sync nor for what Pending
// delivers during it: a context that outgrows the window is the agent's own
// compression's job, as in any reply.
func (im *importer) clear(s *Entry) importResult {
	sum := cloneEntry(s)
	proj := Project(im.entries, &ProjectionSpec{
		Participant: im.ps.name,
		Covered:     cloneCoverage(s.Coverage),
		IncludeOwn:  true,
	})
	return importResult{
		sync:   &Sync{Reset: true, PeerSummary: &sum, Messages: proj.Messages, Cursor: proj.Through},
		tokens: countTokens(proj.Messages),
		clear:  &clearCommit{seq: s.Seq, coverage: cloneCoverage(s.Coverage)},
	}
}

// incremental imports the entries between the cursor and the trigger. Only
// an edge import is budgeted; one that does not fit is applied in chunks
// with a strict compaction after each, the local fallback for when no new
// summary covers the entries. The participant's own entries are not
// projected: it already holds them.
func (im *importer) incremental(st ParticipantState, held *Coverage) (importResult, error) {
	after := st.Cursor
	// After a restart the checkpoint may predate what the participant wrote
	// in its last activation: its own entries based past the cursor come
	// back too.
	var ownAfter uint64
	if im.restored {
		ownAfter = max(after, 1)
	}
	if after+1 >= im.trigger {
		return importResult{budget: im.budgetFor(st)}, nil
	}
	pol := im.c.cfg.Import
	falses := 0
	for round := 0; ; round++ {
		budget := im.budgetFor(st)
		proj := Project(im.entries[min(int(after), len(im.entries)):], &ProjectionSpec{
			Participant:   im.ps.name,
			After:         after,
			Covered:       held,
			MaxTokens:     budget,
			OwnBasedAfter: ownAfter,
		})
		s := &Sync{Messages: proj.Messages, Cursor: proj.Through}
		if !proj.Remaining {
			// The last chunk goes with the activation.
			res := importResult{budget: budget, tokens: countTokens(s.Messages)}
			if !s.Empty() {
				res.sync = s
			}
			return res, nil
		}
		if round >= pol.MaxRounds {
			return im.skip(st, after), nil
		}
		if err := im.ps.p.Sync(im.ctx, *s); err != nil {
			return importResult{}, fmt.Errorf("thread: import into %s: %w", im.ps.name, err)
		}
		after = proj.Through
		ok, err := im.ps.p.Compact(im.ctx, "")
		switch {
		case err != nil:
			return im.skip(im.ps.p.State(), after), nil
		case !ok:
			falses++
			if falses >= 2 {
				return im.skip(im.ps.p.State(), after), nil
			}
		default:
			falses = 0
		}
		st = im.ps.p.State()
	}
}

// skip moves the cursor past the rest of the import with one message that
// names the range, so nothing is imported again and the participant can
// read the range with thread_read.
func (im *importer) skip(st ParticipantState, after uint64) importResult {
	last := im.trigger - 1
	s := &Sync{Cursor: last}
	if after < last {
		var text string
		if after+1 == last {
			text = fmt.Sprintf("[#%d not imported: read it with thread_read]", last)
		} else {
			text = fmt.Sprintf("[#%d–#%d not imported: read them with thread_read]", after+1, last)
		}
		s.Messages = []*message.Msg{projectedMsg(text, last, string(KindMarker))}
	}
	return importResult{sync: s, budget: im.budgetFor(st), tokens: countTokens(s.Messages)}
}

// budgetFor is EdgeShare x Window - InputTokens, at least 1, for an edge
// participant, and 0 (no limit) for any other or when the window is
// unknown: the cloud imports what it lacks in full, and what outgrows its
// window is compacted there.
func (im *importer) budgetFor(st ParticipantState) int {
	if im.ps.desc.Zone != ZoneEdge {
		return 0
	}
	w := st.Window
	if w <= 0 {
		w = im.ps.desc.Window
	}
	if w <= 0 {
		return 0
	}
	return max(int(im.c.cfg.Import.EdgeShare*float64(w))-st.InputTokens, 1)
}

func countTokens(msgs []*message.Msg) int {
	n := 0
	for _, m := range msgs {
		n += estimateTokens(m)
	}
	return n
}

func estimateText(s string) int {
	if s == "" {
		return 0
	}
	return (utf8.RuneCountInString(s) + 3) / 4
}

// pending is Host.Pending: the projection of the entries up to the newest one
// deliverable to the running participant, as a Sync. Deliverable are a
// steering user input or a Notify report addressed to it while it holds the
// foreground, and, to a background activation, the Notify reports of tasks
// that activation assigned itself: a report meant for the idle owner must
// wake a foreground activation, not vanish into a background one. The budget
// is the one its import left, less what Pending already delivered; State is
// not called here because the participant may be inside its own boundary
// callback.
func (c *Coordinator) pending(ctx context.Context, ps *partState, cursor uint64) (*Sync, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := ps.running
	if c.closed || a == nil || !a.started {
		return nil, nil
	}
	entries, err := c.store.Read(ctx, cursor, Filter{}, 0)
	if err != nil {
		return nil, fmt.Errorf("thread: read pending entries for %s: %w", ps.name, err)
	}
	fgOwner := c.holdsForegroundLocked(a)
	x := -1
	for i := range entries {
		e := &entries[i]
		switch e.Kind {
		case KindUserInput:
			if fgOwner && c.undeliveredLocked(e.Seq) {
				x = i
			}
		case KindReport:
			if e.Notify && e.Addressed(ps.name) && (fgOwner || c.ownTaskReportLocked(a, e)) {
				x = i
			}
		}
	}
	if x < 0 {
		return nil, nil
	}
	budget := 0
	if a.limited {
		budget = max(a.budget, 1)
	}
	window := entries[:x+1]
	proj := Project(window, &ProjectionSpec{
		Participant: ps.name,
		After:       cursor,
		Covered:     ps.heldCoverage,
		MaxTokens:   budget,
	})
	if proj.Through <= cursor {
		return nil, nil
	}
	if a.limited {
		a.budget -= countTokens(proj.Messages)
	}
	ps.seen = max(ps.seen, proj.Through)
	if fgOwner {
		c.inputs = slices.DeleteFunc(c.inputs, func(r *inputRec) bool {
			if r.entry.Seq <= proj.Through {
				a.steered = append(a.steered, r)
				return true
			}
			return false
		})
		ps.deliveredNotifyLocked(proj.Through)
	} else {
		// Reports of its own tasks reached the activation they were meant
		// for; any other Notify report it saw as context still waits for a
		// foreground activation.
		for i := range window {
			if e := &window[i]; e.Seq <= proj.Through && c.ownTaskReportLocked(a, e) {
				delete(ps.notify, e.Seq)
			}
		}
	}
	return &Sync{Messages: proj.Messages, Cursor: proj.Through}, nil
}

// ownTaskReportLocked reports whether e reports on a task activation a
// assigned itself.
func (c *Coordinator) ownTaskReportLocked(a *activation, e *Entry) bool {
	if e.Kind != KindReport || e.Task == nil {
		return false
	}
	tk := c.tasks[e.BasedOn]
	return tk != nil && tk.id == e.Task.ID && tk.assignerAct == a.id
}
