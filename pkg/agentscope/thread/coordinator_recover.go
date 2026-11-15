package thread

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// maxRestarts bounds how often recovery resumes one request: a request
// whose run keeps stopping the process waits for the user after it.
// Restart markers are keyed by the leaf they resume; above 2, a resume after
// a requeue could reuse a key, so raising it means keying them by count as
// requeue markers are.
const maxRestarts = 2

// recoveryNoteRunes bounds the host's note a restarted marker records.
const recoveryNoteRunes = 1000

// Dedup key prefixes of the markers recovery writes. A restarted marker
// keyed restartPrefix+leaf resumes a request on its owner and becomes its
// leaf; one keyed requeuePrefix+request+":"+restart routes it again, and
// the request starts over. A recovered marker is keyed by the leaf it ends.
const (
	restartPrefix   = "restart:"
	requeuePrefix   = "requeue:"
	recoveredPrefix = "recovered:"
)

// reqChain is one user request and the entries that carried it on: the
// owner changes that handed its run on (transfers, failovers) and the
// restarted markers based on it or on each other. leaf is the newest of
// them; only entries based on the leaf say how the request stands.
type reqChain struct {
	root     *Entry
	leaf     uint64
	last     uint64 // the newest entry based on one of its seqs or listing it in Steered, else the request
	touched  bool   // some entry is based on one of its seqs: it was routed, ran or was recovered
	replied  bool   // a reply is based on the leaf
	failed   bool   // a failed marker is based on the leaf
	ended    bool   // an interrupted, no_owner or recovered marker is
	absorbed bool   // a reply or transfer took it as steering, or a run outside any request did
	// joinTo is the request whose failed or interrupted run took it as
	// steering (Entry.Steered): it ends when that request ends unanswered
	// and otherwise shares its fate at recovery.
	joinTo   *reqChain
	restarts int
	lastFail uint64   // the newest failed marker based on any of its seqs
	progress *Entry   // the newest live progress entry based on any of its seqs
	with     []*Entry // the inputs recovery joins to it, in log order
}

// settled reports whether the request's own entries finished it. A reply
// finishes it unless the run it ends failed: a failed run whose failover
// was never written is still unanswered.
func (ch *reqChain) settled() bool {
	return ch.absorbed || ch.ended || (ch.replied && !ch.failed)
}

// done reports whether the request needs nothing more from recovery: it is
// settled, or the request whose run took it ended unanswered.
func (ch *reqChain) done() bool {
	return ch.settled() || (ch.joinTo != nil && ch.top().ended)
}

// follows reports whether ch shares the fate of the open request whose run
// took it as steering.
func (ch *reqChain) follows() bool {
	return ch.joinTo != nil && !ch.top().settled()
}

// top is the request the joins from ch lead to.
func (ch *reqChain) top() *reqChain {
	t := ch
	for t.joinTo != nil {
		t = t.joinTo
	}
	return t
}

// reaches reports whether the joins from ch pass other.
func (ch *reqChain) reaches(other *reqChain) bool {
	for t := ch; t != nil; t = t.joinTo {
		if t == other {
			return true
		}
	}
	return false
}

// extend makes seq the chain's leaf.
func (ch *reqChain) extend(seq uint64) {
	ch.leaf, ch.replied, ch.failed = seq, false, false
}

// join adds o's request to the inputs resumed with ch, and its failure and
// progress when they are newer.
func (ch *reqChain) join(o *reqChain) {
	ch.with = append(ch.with, o.root)
	ch.lastFail = max(ch.lastFail, o.lastFail)
	if p := o.progress; p != nil && (ch.progress == nil || p.Seq > ch.progress.Seq) {
		ch.progress = p
	}
}

// newestOf returns the newest request of chs some entry is based on, else
// the newest one; nil when chs is empty.
func newestOf(chs []*reqChain) *reqChain {
	var best *reqChain
	for _, ch := range chs {
		if best == nil || ch.touched || !best.touched {
			best = ch
		}
	}
	return best
}

// recover rebuilds the Coordinator's state from the log and the
// participants, then carries on what the previous process left unfinished
// (see planRecovery). A task with no final report gets a report to its
// assigner saying it did not finish, pointing at its newest progress. Every
// participant's first import brings back what it wrote past its
// checkpoint's cursor. A participant whose cursor is past the log head
// fails recovery: its checkpoint does not belong to this log.
func (c *Coordinator) recover(ctx context.Context) error {
	head, err := c.store.Head(ctx)
	if err != nil {
		return fmt.Errorf("thread: read log head: %w", err)
	}
	entries, err := c.store.Read(ctx, 0, Filter{}, 0)
	if err != nil {
		return fmt.Errorf("thread: read log: %w", err)
	}
	c.owner, c.epoch, c.lastSeq = head.Owner, head.Epoch, head.LastSeq

	states := make(map[string]ParticipantState, len(c.order))
	for _, ps := range c.order {
		st := ps.p.State()
		if st.Cursor > head.LastSeq {
			return fmt.Errorf("thread: participant %s has cursor %d but the log ends at #%d; "+
				"its checkpoint does not belong to this log, or the log lost entries (for example ones written WithoutSync)",
				ps.name, st.Cursor, head.LastSeq)
		}
		states[ps.name] = st
	}

	reported := make(map[string]bool)
	chains := make(map[uint64]*reqChain) // every seq of a chain -> the chain
	var order []*reqChain
	stale := staleProgress(entries)
	taskProgress := make(map[uint64]*Entry) // assign seq -> newest live progress
	for i := range entries {
		e := &entries[i]
		if ps := c.parts[e.Origin.Participant]; ps != nil {
			ps.lastActivity = e.Seq
			if sealedOutput(e) {
				ps.lastSealed = e.Seq
			}
		}
		switch e.Kind {
		case KindUserInput:
			ch := &reqChain{root: e, leaf: e.Seq, last: e.Seq}
			chains[e.Seq] = ch
			order = append(order, ch)
		case KindReport:
			// Only the report that ends a task counts, never a progress
			// report about it.
			if e.Task != nil && e.Task.End != "" {
				reported[e.Task.ID] = true
			}
		case KindAssign:
			if e.Task != nil {
				if n, ok := strings.CutPrefix(e.Task.ID, "t-"); ok {
					if v, err := strconv.Atoi(n); err == nil {
						c.nextTask = max(c.nextTask, v)
					}
				}
			}
		case KindProgress:
			if !stale[e.Seq] {
				taskProgress[e.BasedOn] = e
			}
		}
		recordChainEntry(chains, e, stale)
	}

	for _, ps := range c.order {
		st := states[ps.name]
		ps.seen = st.Cursor
		ps.lastInput = st.InputTokens
		ps.restored = true
		for i := range entries {
			e := &entries[i]
			// Reports past the cursor reached no foreground activation;
			// they still wake the owner.
			if e.Seq > st.Cursor && e.Kind == KindReport && e.Notify && e.Addressed(ps.name) {
				if ps.notify == nil {
					ps.notify = make(map[uint64]*Entry)
				}
				cp := cloneEntry(e)
				ps.notify[e.Seq] = &cp
			}
		}
	}

	plan := c.planRecovery(order)
	batch := plan.batch
	for i := range entries {
		e := &entries[i]
		if e.Kind != KindAssign || e.Task == nil || reported[e.Task.ID] || len(e.To) == 0 {
			continue
		}
		text := fmt.Sprintf("Task %s did not finish before the session stopped; its effects are unknown.", e.Task.ID)
		if p := taskProgress[e.Seq]; p != nil {
			text += fmt.Sprintf(" Its last progress is #%d.", p.Seq)
		}
		batch = append(batch, Entry{
			Kind:     KindReport,
			To:       []string{e.Origin.Participant},
			BasedOn:  e.Seq,
			Task:     &Task{ID: e.Task.ID, End: TaskUnfinished},
			Content:  textContent(text),
			DedupKey: orphanPrefix + e.Task.ID,
		})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var stored []Entry
	if len(batch) > 0 {
		if stored, err = c.appendLocked(batch, nil); err != nil {
			return fmt.Errorf("thread: record recovery: %w", err)
		}
		c.emitEntriesLocked(stored)
	}
	for _, ch := range plan.requeue {
		c.queueInputLocked(&inputRec{entry: cloneEntry(ch.root), q: recoveredQuery(ch.root)})
	}
	switch {
	case plan.resume != nil:
		c.resumeLocked(plan.resume, stored)
	case plan.waiting != nil:
		in := &inputRec{entry: cloneEntry(plan.waiting.root), q: recoveredQuery(plan.waiting.root)}
		c.queueInputLocked(in)
		c.parkLocked(in)
	}
	c.maybeStartCycleLocked()
	return nil
}

// recoveryPlan is what recovery writes and starts for the open requests.
type recoveryPlan struct {
	batch   []Entry
	resume  *reqChain   // resumed by the owner
	waiting *reqChain   // parked after maxRestarts
	requeue []*reqChain // routed again, in log order
}

// recoveryFate is what recovery does with one open request.
type recoveryFate int

const (
	fateRequeue   recoveryFate = iota // routed again, as Submit would queue it
	fateRecovered                     // a recovered marker; not run again
	fateResume                        // resumed by the owner behind a restarted marker
	fateJoin                          // resumed with the request the owner resumes
	fateRetry                         // routed again behind a restarted marker
	fatePark                          // a recovered marker, then waits for the next Submit
)

// planRecovery decides what happens to the requests the log leaves open.
//
// The turn the stopped process was answering holds every open request no
// entry is based on, and the open requests with an entry after the newest
// finished one. A request that waited for the previous run and then ran on
// the same owner leaves no entry before its run ends (the router writes an
// owner change only when the owner changes), so a request with nothing
// based on it may be in flight wherever it stands in the log. A request
// whose entries all precede the newest finished one ran in an earlier turn
// and was left unfinished. The owner resumes the turn's requests: the
// newest one some entry is based on (an owner change, progress, a marker),
// else the newest, with the others joined to it. A restarted marker records
// the restart (with Config.RecoveryNote), and the owner gets an activation
// whose input quotes those requests and points at the failure and the
// newest progress the log holds, warning that work done before may or may
// not have taken effect (nothing is re-run on its own). The owner is the
// participant the log says was answering: a routed request's owner change
// precedes its activation. When there is no registered owner (or no turn
// request), the turn's routing never finished, so nothing ran: its request
// is routed again behind a restarted marker that counts the restart.
//
// An open request outside the turn gets a recovered marker and is not
// re-run when something is based on it, and is routed again as Submit would
// queue it otherwise. A request resumed maxRestarts times gets a recovered
// marker, as do the inputs that would have joined it, and waits for the
// next Submit. An input that a failed or interrupted run took as steering
// (Entry.Steered) shares the fate of that run's request.
func (c *Coordinator) planRecovery(order []*reqChain) recoveryPlan {
	var lastDone uint64
	for _, ch := range order {
		if ch.done() {
			lastDone = max(lastDone, ch.last)
		}
	}
	var turn []*reqChain
	for _, ch := range order {
		if !ch.done() && !ch.follows() && (!ch.touched || ch.last > lastDone) {
			turn = append(turn, ch)
		}
	}
	base := newestOf(turn)
	resumable := base != nil && c.parts[c.owner] != nil
	retry := base != nil && base.restarts < maxRestarts

	fates := make(map[*reqChain]recoveryFate)
	for _, ch := range order {
		if ch.done() || ch.follows() {
			continue
		}
		inTurn := slices.Contains(turn, ch)
		switch {
		case ch == base && !retry:
			fates[ch] = fatePark
		case ch == base && resumable:
			fates[ch] = fateResume
		case ch == base:
			fates[ch] = fateRetry
		case resumable && inTurn && retry:
			fates[ch] = fateJoin
		case resumable && inTurn, ch.touched:
			fates[ch] = fateRecovered
		default:
			fates[ch] = fateRequeue
		}
	}
	for _, ch := range order {
		if ch.done() || !ch.follows() {
			continue
		}
		switch fates[ch.top()] {
		case fateResume, fateJoin:
			fates[ch] = fateJoin
		case fateRecovered, fatePark:
			fates[ch] = fateRecovered
		default:
			fates[ch] = fateRequeue
		}
	}

	var p recoveryPlan
	var joined []*reqChain
	for _, ch := range order {
		f, ok := fates[ch]
		if !ok {
			continue
		}
		switch f {
		case fateResume:
			p.resume = ch
		case fateJoin:
			joined = append(joined, ch)
		case fateRetry:
			p.requeue = append(p.requeue, ch)
			p.batch = append(p.batch, c.restartedMarker(ch, requeuePrefix))
		case fateRequeue:
			p.requeue = append(p.requeue, ch)
		case fatePark:
			p.waiting = ch
			p.batch = append(p.batch, recoveredMarker(ch))
		case fateRecovered:
			p.batch = append(p.batch, recoveredMarker(ch))
		}
	}
	if p.resume != nil {
		for _, ch := range joined {
			p.resume.join(ch)
		}
		p.batch = append(p.batch, c.restartedMarker(p.resume, restartPrefix))
	}
	return p
}

// recordChainEntry applies one log entry to the request chains.
func recordChainEntry(chains map[uint64]*reqChain, e *Entry, stale map[uint64]bool) {
	if e.Kind == KindUserInput {
		return
	}
	ch := chains[e.BasedOn]
	recordSteered(chains, ch, e)
	if ch == nil {
		return
	}
	ch.touched = true
	ch.last = max(ch.last, e.Seq)
	switch e.Kind {
	case KindOwnerChange:
		// The router's owner change precedes the run of the request it is
		// based on; that run's entries stay based on the request.
		if e.Owner != nil && e.Owner.Reason == ReasonRouter {
			return
		}
		chains[e.Seq] = ch
		ch.extend(e.Seq)
	case KindReply:
		if e.BasedOn == ch.leaf {
			ch.replied = true
		}
	case KindProgress:
		if !stale[e.Seq] {
			ch.progress = e
		}
	case KindMarker:
		if e.Marker == nil {
			return
		}
		switch e.Marker.Kind {
		case MarkerRestarted:
			ch.restarts++
			switch {
			case strings.HasPrefix(e.DedupKey, restartPrefix):
				chains[e.Seq] = ch
				ch.extend(e.Seq)
			case strings.HasPrefix(e.DedupKey, requeuePrefix):
				ch.extend(ch.root.Seq)
			}
		case MarkerFailed:
			ch.lastFail = e.Seq
			if e.BasedOn == ch.leaf {
				ch.failed = true
			}
		case MarkerInterrupted, MarkerNoOwner, MarkerRecovered:
			if e.BasedOn == ch.leaf {
				ch.ended = true
			}
		}
	}
}

// recordSteered applies the inputs e lists as taken by the run it ends
// (Entry.Steered). A reply or a transfer answered them; a failed or
// interrupted run of request ch leaves them to ch's fate. A run outside any
// request (a wake) took them for good when it was interrupted; when it
// failed it queued them again, so they stay open.
func recordSteered(chains map[uint64]*reqChain, ch *reqChain, e *Entry) {
	for _, s := range e.Steered {
		other := chains[s]
		if other == nil || other.root.Seq != s || other == ch {
			continue
		}
		other.last = max(other.last, e.Seq)
		switch {
		case e.Kind == KindReply || e.Kind == KindOwnerChange:
			other.absorbed = true
		case ch == nil && e.Kind == KindMarker && e.Marker.Kind == MarkerFailed:
		case ch == nil:
			other.absorbed = true
		case !ch.reaches(other):
			other.joinTo = ch
		}
	}
}

// restartedMarker records the restart on ch: based on its leaf when the
// owner resumes it (restartPrefix, which makes the marker the leaf), on its
// request when it is routed again (requeuePrefix, keyed by the restart's
// number, so every restart writes its own). It carries the host's note.
func (c *Coordinator) restartedMarker(ch *reqChain, prefix string) Entry {
	on := ch.leaf
	key := prefix + strconv.FormatUint(on, 10)
	if prefix == requeuePrefix {
		on = ch.root.Seq
		key = fmt.Sprintf("%s%d:%d", requeuePrefix, on, ch.restarts+1)
	}
	what := "#" + strconv.FormatUint(ch.root.Seq, 10)
	for _, e := range ch.with {
		what += ", #" + strconv.FormatUint(e.Seq, 10)
	}
	detail := fmt.Sprintf("the session restarted while %s was being answered; work done before the restart may or may not have taken effect", what)
	if note := truncateRunes(strings.TrimSpace(c.cfg.RecoveryNote), recoveryNoteRunes); note != "" {
		detail += ". Host: " + note
	}
	mk := markerEntry(MarkerRestarted, on, detail)
	mk.DedupKey = key
	return mk
}

// recoveredMarker ends ch at its leaf without running it again.
func recoveredMarker(ch *reqChain) Entry {
	mk := markerEntry(MarkerRecovered, ch.leaf, fmt.Sprintf(
		"#%d was not answered before the session stopped; its effects are unknown, so it was not run again", ch.root.Seq))
	mk.DedupKey = recoveredPrefix + strconv.FormatUint(ch.leaf, 10)
	return mk
}

// resumeLocked starts the owner's activation that resumes ch, triggered by
// its restarted marker among stored.
func (c *Coordinator) resumeLocked(ch *reqChain, stored []Entry) {
	key := restartPrefix + strconv.FormatUint(ch.leaf, 10)
	var trigger *Entry
	for i := range stored {
		if stored[i].DedupKey == key {
			trigger = &stored[i]
		}
	}
	if trigger == nil {
		return
	}
	ps := c.parts[c.owner]
	c.cycle = cycleState{input: ch.root.Seq, text: ch.root.Text()}
	a := c.newActivationLocked(ps, actRecover, trigger, true, 0)
	a.cycleInput = ch.root.Seq
	a.userText = ch.root.Text()
	a.recoverWith = ch.with
	for _, e := range ch.with {
		a.steered = append(a.steered, &inputRec{entry: cloneEntry(e), q: recoveredQuery(e)})
	}
	if ch.lastFail != 0 {
		a.failSeq = ch.lastFail
	}
	if p := ch.progress; p != nil {
		a.progress = p.Seq
		cp := cloneEntry(p)
		c.cycle.progress = &cp
	}
	c.startForegroundLocked(a)
}

// recoveredQuery restores the query of a request from its entry.
func recoveredQuery(e *Entry) Query {
	return Query{Text: e.Text()}
}

// orphanPrefix starts the dedup key of the report recovery writes for a task
// that never finished.
const orphanPrefix = "orphan:"

// sealedOutput reports whether e is a participant's own sealed output: a
// reply, an owner change it wrote (a Transfer), or the report that ended a
// task it ran.
func sealedOutput(e *Entry) bool {
	switch e.Kind {
	case KindReply:
		// An empty reply only marks its request answered; a summary gains
		// nothing from it.
		return strings.TrimSpace(e.Text()) != "" || cloneDigest(e.Digest) != nil
	case KindOwnerChange:
		return true
	case KindReport:
		return e.Task != nil && e.Task.End != "" && e.Task.End != TaskUnfinished
	}
	return false
}
