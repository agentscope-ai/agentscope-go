package thread

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// forkJob is one background summary job of a Forker participant: a peer
// fork, an own fork, or both, on the same snapshot.
type forkJob struct {
	id     string
	ps     *partState
	snap   Snapshot
	growth int // growth when the job started; a successful peer fork subtracts it
	// through is the participant's last sealed output at the snapshot: what
	// a peer summary must cover before the own notes may be offered.
	through uint64

	peer     bool
	target   *partState
	budget   int
	coverage Coverage
	peerSpec ForkSpec

	own     bool
	ownBase string // the notes the own fork updates, "" for the first ones
	ownSpec ForkSpec
}

// afterEnd decides whether the participant's sealed activation calls for a
// peer summary or own notes and starts the job. It runs without the mutex.
func (c *Coordinator) afterEnd(a *activation, reason types.ReplyFinishedReason, started bool) {
	ps := a.part
	if ps.forker == nil {
		return
	}
	if c.cfg.Summaries.Disabled || !started || reason == types.ReplyError {
		return
	}
	job := c.planForks(a)
	if job == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || ps.forking {
		return
	}
	ps.forking = true
	c.nextJob++
	job.id = fmt.Sprintf("%s-f%d", c.runID, c.nextJob)
	c.addWorkLocked()
	c.wg.Add(1)
	go c.runForkJob(job)
}

// planForks updates the growth counter and returns the job the activation
// calls for, or nil.
func (c *Coordinator) planForks(a *activation) *forkJob {
	ps := a.part
	pol := &c.cfg.Summaries
	st := ps.p.State()

	c.mu.Lock()
	ps.growth += max(0, st.InputTokens-ps.lastInput)
	ps.lastInput = st.InputTokens
	growth, through, handedTo, busy := ps.growth, ps.lastSealed, a.handedTo, ps.forking
	needOwn := !busy && c.ownDueLocked(ps, st.InputTokens)
	base := ps.ownNotes
	c.mu.Unlock()
	if busy {
		return nil
	}

	target := c.parts[c.edge]
	needPeer := false
	if target != nil && target != ps && through > 0 {
		// A new peer summary only pays for itself when the writer sealed
		// something past the newest one's coverage: growth from work that
		// sealed nothing new would buy the same summary again.
		prev := c.newestSummary(c.ctx, target.name)
		stale := prev == nil || prev.Coverage.Through < through
		needPeer = stale && (handedTo == target.name || c.peerDue(ps, target, prev, growth))
	}
	// Own notes replace the history they cover once the participant reaches
	// its compaction line, so the tool work in it could no longer reach a
	// peer summary: notes are offered only once a peer summary covers their
	// snapshot. Near the line, a stale peer summary is written from the
	// notes' snapshot so the notes the line installs are the newest.
	if needOwn && !needPeer && target != nil && target != ps && through > 0 && nearCompaction(&st, pol.OwnEvery) {
		prev := c.newestSummary(c.ctx, target.name)
		needPeer = prev == nil || prev.Coverage.Through < through
	}
	if !needPeer && !needOwn {
		return nil
	}
	snap, err := ps.forker.Snapshot()
	if err != nil {
		if !errors.Is(err, ErrNoSnapshot) {
			agentscope.Log().WithError(err).WithField("participant", ps.name).Warn("thread: snapshot for summary fork")
		}
		return nil
	}
	job := &forkJob{ps: ps, snap: snap, growth: growth, through: through}
	if needPeer {
		if err := c.planPeer(job, target, through); err != nil {
			agentscope.Log().WithError(err).WithField("participant", ps.name).Warn("thread: plan peer summary")
		}
	}
	if needOwn {
		job.own = true
		job.ownBase = base
		job.ownSpec = ForkSpec{Prompt: pol.OwnPrompt, MaxTokens: pol.OwnMaxTokens,
			Validate: notesValidator(base, pol.OwnMaxTokens), MaxRewrites: pol.MaxRewrites}
		if base != "" {
			job.ownSpec.Prompt = pol.OwnUpdatePrompt
			job.ownSpec.Appendix = "Your notes so far:\n" + base
		}
		c.mu.Lock()
		ps.ownAt = st.InputTokens
		c.mu.Unlock()
	}
	if !job.peer && !job.own {
		return nil
	}
	return job
}

// ownDueLocked applies Claude Code's session-memory schedule at the end of a
// reply: the first notes once the input reaches OwnInit, then an update
// whenever it grew OwnEvery since the last own fork started. Claude Code
// also waits for three tool calls when a turn's last response called tools;
// a reply ends on a response that called none, so that never applies here.
// A compaction since the last fork lowers the baseline to the new input.
func (c *Coordinator) ownDueLocked(ps *partState, input int) bool {
	pol := &c.cfg.Summaries
	if input < ps.ownAt {
		ps.ownAt = input
		return false
	}
	if input < pol.OwnInit {
		return false
	}
	return ps.ownAt == 0 || input-ps.ownAt >= pol.OwnEvery
}

// nearCompaction reports whether the participant may reach its compaction
// line before its next own update: its input is within one update interval
// of the line. An unknown line counts as near.
func nearCompaction(st *ParticipantState, every int) bool {
	return st.CompactAt <= 0 || st.InputTokens+every >= st.CompactAt
}

// planPeer fills the peer fork of job: its coverage, recorded before the
// fork, and its spec. The coverage excludes the entries of others past the
// snapshot's cursor: the writer imports what it lacks in full (its imports
// are not budgeted), so everything else up to its cursor is in the
// snapshot.
func (c *Coordinator) planPeer(job *forkJob, target *partState, through uint64) error {
	pol := &c.cfg.Summaries
	entries, err := c.store.Read(c.ctx, 0, Filter{}, int(through))
	if err != nil {
		return err
	}
	cursor := job.snap.Cursor()
	cov := Coverage{Through: through}
	for i := range entries {
		e := &entries[i]
		if e.Seq > cursor && e.Origin.Participant != job.ps.name {
			cov.Excluded = append(cov.Excluded, e.Seq)
		}
	}
	budget := peerBudget(&target.desc, target.p.State())
	maxTokens := pol.PeerMaxTokens
	if maxTokens <= 0 {
		maxTokens = max(2*budget, 512)
	}
	window := "unknown"
	if target.desc.Window > 0 {
		window = strconv.Itoa(target.desc.Window)
	}
	prompt := strings.NewReplacer("{target}", target.name, "{window}", window, "{budget}", strconv.Itoa(budget)).Replace(pol.PeerPrompt)
	var appendix string
	if prev := c.newestSummary(c.ctx, target.name); prev != nil {
		appendix = "Previous summary for " + target.name + ", update it:\n" + prev.Text()
	}
	job.peer = true
	job.target = target
	job.budget = budget
	job.coverage = cov
	job.peerSpec = ForkSpec{
		Prompt:      prompt,
		Appendix:    appendix,
		MaxTokens:   maxTokens,
		Validate:    peerValidator(budget),
		MaxRewrites: pol.MaxRewrites,
	}
	return nil
}

// peerDue reports whether the writer ps must refresh target's summary at the
// end of this reply: its growth reached a positive PeerEvery, or the
// takeover load, the newest summary plus the entries after it that a clear
// would bring, will reach PeerRatio of target's window by the end of the
// next reply, judged by how much this reply added. Without a known window,
// PeerEvery's default decides.
func (c *Coordinator) peerDue(ps, target *partState, prev *Entry, growth int) bool {
	pol := &c.cfg.Summaries
	if pol.PeerEvery > 0 && growth >= pol.PeerEvery {
		return true
	}
	if pol.PeerRatio < 0 {
		return false
	}
	w := target.p.State().Window
	if w <= 0 {
		w = target.desc.Window
	}
	if w <= 0 {
		return pol.PeerEvery == 0 && growth >= defaultPeerEvery
	}
	load, ok := c.handoverLoad(target, prev)
	if !ok {
		return false
	}
	c.mu.Lock()
	rise := max(load-ps.peerLoad, 0)
	ps.peerLoad = load
	c.mu.Unlock()
	return float64(load+rise) >= pol.PeerRatio*float64(w)
}

// handoverLoad estimates what a clear would bring target now: the newest
// summary written for it and every entry after that summary it may read
// (its own included, which a clear also brings). It reads the store.
func (c *Coordinator) handoverLoad(target *partState, prev *Entry) (int, bool) {
	var after uint64
	n := 0
	if prev != nil {
		after = prev.Coverage.Through
		n = estimateText(prev.Text())
	}
	entries, err := c.store.Read(c.ctx, after, Filter{}, 0)
	if err != nil {
		agentscope.Log().WithError(err).WithField("participant", target.name).Warn("thread: read entries for the peer summary rule")
		return 0, false
	}
	proj := Project(entries, &ProjectionSpec{Participant: target.name, After: after, IncludeOwn: true})
	return n + countTokens(proj.Messages), true
}

// peerBudget is the target size of a summary written for a participant:
// its Descriptor.SummaryBudget, else 5/6 of the summary budget its State
// reports (stage 0 gave 2,500 of 3,000 tokens to the peer summary), else
// 5/16 of its window (5/6 of the agent's automatic 3/8), else 1,024; at
// least 256.
func peerBudget(d *Descriptor, st ParticipantState) int {
	w := st.Window
	if w <= 0 {
		w = d.Window
	}
	switch {
	case d.SummaryBudget > 0:
		return d.SummaryBudget
	case st.SummaryTokens > 0:
		return max(st.SummaryTokens*5/6, 256)
	case w > 0:
		return max(w*5/16, 256)
	}
	return defaultPeerBudget
}

// peerValidator accepts a summary of at most 1.5 x budget estimated tokens.
func peerValidator(budget int) func(string) (string, bool) {
	return func(text string) (string, bool) {
		n := estimateText(text)
		if float64(n) <= 1.5*float64(budget) {
			return "", true
		}
		return fmt.Sprintf("Too long (%d tokens). Rewrite it under %d tokens.", n, budget), false
	}
}

// runForkJob runs the job's forks concurrently, appends the peer summary,
// then offers the own notes once the peer fork settled.
func (c *Coordinator) runForkJob(job *forkJob) {
	defer c.wg.Done()
	ps := job.ps
	var peerOut, ownOut ForkOutput
	var peerErr, ownErr error
	var wg sync.WaitGroup
	if job.peer {
		wg.Go(func() { peerOut, peerErr = ps.forker.Fork(c.ctx, job.snap, job.peerSpec) })
	}
	if job.own {
		wg.Go(func() { ownOut, ownErr = ps.forker.Fork(c.ctx, job.snap, job.ownSpec) })
	}
	wg.Wait()
	if job.peer {
		c.finishPeer(job, &peerOut, peerErr)
	}
	if job.own {
		c.finishOwn(job, &ownOut, ownErr)
	}
	c.mu.Lock()
	ps.forking = false
	c.doneWorkLocked()
	c.mu.Unlock()
}

// finishPeer records the peer fork and appends its summary.
func (c *Coordinator) finishPeer(job *forkJob, out *ForkOutput, err error) {
	ps := job.ps
	c.recordForkUsage(ps.name, UsageForkPeer, out, err)
	if err != nil {
		agentscope.Log().WithError(err).WithField("participant", ps.name).Warn("thread: peer summary fork")
		return
	}
	text := strings.TrimSpace(out.Text)
	if text == "" {
		agentscope.Log().WithField("participant", ps.name).Warn("thread: peer summary fork returned no text")
		return
	}
	if out.Rejected {
		text = middleCut(text, job.budget)
	}
	cov := job.coverage
	e := Entry{
		Kind:     KindSummary,
		Origin:   Origin{Participant: ps.name},
		Summary:  &SummaryMeta{Target: job.target.name, Budget: job.budget, Lossy: out.Rejected},
		Coverage: &cov,
		Content:  textContent(text),
		DedupKey: "peer:" + job.id,
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if c.commitLocked(ps.name, []Entry{e}, nil) != nil {
		ps.growth = max(0, ps.growth-job.growth)
	}
}

// finishOwn records the own fork, keeps its notes and offers them to the
// participant once a peer summary covers their snapshot. Notes whose edits
// do not apply are dropped; notes over the limit after the rewrites are cut
// in the middle. It runs without the mutex.
func (c *Coordinator) finishOwn(job *forkJob, out *ForkOutput, err error) {
	ps := job.ps
	c.recordForkUsage(ps.name, UsageForkOwn, out, err)
	if err != nil {
		agentscope.Log().WithError(err).WithField("participant", ps.name).Warn("thread: own summary fork")
		return
	}
	notes, err := applyNoteEdits(job.ownBase, out.Text)
	if err != nil {
		agentscope.Log().WithError(err).WithField("participant", ps.name).Warn("thread: own notes update does not apply")
		return
	}
	if limit := c.cfg.Summaries.OwnMaxTokens; estimateText(notes) > limit {
		notes = middleCut(notes, limit)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	ps.ownNotes = notes
	c.mu.Unlock()
	if target := c.parts[c.edge]; target != nil && target != ps && job.through > 0 {
		if prev := c.newestSummary(c.ctx, target.name); prev == nil || prev.Coverage.Through < job.through {
			// The participant keeps the notes it was offered last; these
			// become the base of the next update.
			return
		}
	}
	err = ps.forker.OfferOwnSummary(c.ctx, job.snap, notes)
	switch {
	case errors.Is(err, ErrStale):
		agentscope.Log().WithError(err).WithField("participant", ps.name).Info("thread: own notes are stale; not offered")
	case err != nil:
		agentscope.Log().WithError(err).WithField("participant", ps.name).Warn("thread: offer own notes")
	}
}

// recordForkUsage records a fork's calls: one entry with its usage when it
// is known, otherwise one entry with nil usage per call made.
func (c *Coordinator) recordForkUsage(name string, kind UsageKind, out *ForkOutput, err error) {
	now := c.cfg.Clock()
	switch {
	case out.Usage != nil:
		c.ledger.Record(LedgerEntry{Participant: name, Kind: kind, Usage: out.Usage, Time: now})
	case out.Calls > 0:
		for range out.Calls {
			c.ledger.Record(LedgerEntry{Participant: name, Kind: kind, Time: now})
		}
	case err == nil:
		c.ledger.Record(LedgerEntry{Participant: name, Kind: kind, Time: now})
	}
}

// middleCut keeps the head and tail of text within budget estimated tokens,
// marking the cut.
func middleCut(text string, budget int) string {
	const mark = "\n[…]\n"
	maxRunes := budget * 4
	r := []rune(text)
	if len(r) <= maxRunes {
		return text
	}
	keep := maxRunes - utf8.RuneCountInString(mark)
	if keep <= 0 {
		return string(r[:max(maxRunes, 0)])
	}
	head := keep / 2
	tail := keep - head
	return string(r[:head]) + mark + string(r[len(r)-tail:])
}
