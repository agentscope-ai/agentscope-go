package thread

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
)

// maybeStartCycleLocked starts a query cycle for the newest undelivered,
// unparked input when the foreground is free. Older inputs stay queued until
// the cycle's owner is known; they are then dropped as context its
// activation imports.
func (c *Coordinator) maybeStartCycleLocked() {
	if c.closed || c.fg != nil || c.routing {
		return
	}
	idx := -1
	for i := len(c.inputs) - 1; i >= 0; i-- {
		if !c.inputs[i].parked {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	in := c.inputs[idx]
	c.cycle = cycleState{input: in.entry.Seq, text: in.entry.Text()}
	c.routing = true
	ctx, cancel := context.WithCancel(c.ctx)
	c.fgCancel = cancel
	c.addWorkLocked()
	c.wg.Add(1)
	go c.runCycle(ctx, cancel, in)
}

// waitingInputsLocked counts the inputs still waiting for a query cycle: the
// unparked ones.
func (c *Coordinator) waitingInputsLocked() int {
	n := 0
	for _, r := range c.inputs {
		if !r.parked {
			n++
		}
	}
	return n
}

// parkLocked parks in until a later cycle carries it, and retires the oldest
// parked inputs beyond MaxPendingInputs: they stay in the log as context, so
// nothing is lost, but they no longer wait (an input parked while no
// participant could take it would otherwise wait forever).
func (c *Coordinator) parkLocked(in *inputRec) {
	in.parked = true
	parked := 0
	for _, r := range c.inputs {
		if r.parked {
			parked++
		}
	}
	for i := 0; parked > c.cfg.Limits.MaxPendingInputs && i < len(c.inputs); {
		if c.inputs[i].parked {
			c.inputs = slices.Delete(c.inputs, i, i+1)
			parked--
			continue
		}
		i++
	}
}

// queueInputLocked adds in to the undelivered inputs, keeping them in log
// order.
func (c *Coordinator) queueInputLocked(in *inputRec) {
	i, _ := slices.BinarySearchFunc(c.inputs, in.entry.Seq, func(r *inputRec, seq uint64) int {
		return cmp.Compare(r.entry.Seq, seq)
	})
	c.inputs = slices.Insert(c.inputs, i, in)
}

// removeInputLocked drops in from the undelivered inputs.
func (c *Coordinator) removeInputLocked(in *inputRec) {
	c.inputs = slices.DeleteFunc(c.inputs, func(r *inputRec) bool { return r == in })
}

// olderInputsLocked returns the unparked inputs older than in: the ones its
// cycle would carry as context.
func (c *Coordinator) olderInputsLocked(in *inputRec) []*inputRec {
	var out []*inputRec
	for _, r := range c.inputs {
		if r != in && !r.parked && r.entry.Seq < in.entry.Seq {
			out = append(out, r)
		}
	}
	return out
}

// dropContextInputsLocked settles the inputs older than in once its cycle
// started: they are dropped, because its activation imports them, and
// returned so the entry that ends its run records them (Entry.Steered).
func (c *Coordinator) dropContextInputsLocked(in *inputRec) []*inputRec {
	var dropped []*inputRec
	c.inputs = slices.DeleteFunc(c.inputs, func(r *inputRec) bool {
		if r.entry.Seq >= in.entry.Seq {
			return false
		}
		dropped = append(dropped, r)
		return true
	})
	return dropped
}

// undeliveredLocked reports whether the user input seq is still waiting.
func (c *Coordinator) undeliveredLocked(seq uint64) bool {
	return slices.ContainsFunc(c.inputs, func(r *inputRec) bool { return r.entry.Seq == seq })
}

// runCycle routes one input and starts its foreground activation.
func (c *Coordinator) runCycle(ctx context.Context, cancel context.CancelFunc, in *inputRec) {
	defer c.wg.Done()
	defer cancel()
	req, avail := c.routeRequest(ctx, in)
	dec, err := c.route(ctx, &req, avail)
	c.recordRouterUsage(&dec)

	c.mu.Lock()
	reconcile := c.finishRoutingLocked(ctx, in, &dec, err)
	c.mu.Unlock()
	if reconcile != nil {
		c.reconcile(reconcile)
	}
	c.mu.Lock()
	c.doneWorkLocked()
	c.mu.Unlock()
}

// finishRoutingLocked commits the routing outcome. It returns the owner when
// no activation started, so entries for it can be checked.
func (c *Coordinator) finishRoutingLocked(ctx context.Context, in *inputRec, dec *RouteDecision, err error) *partState {
	c.routing = false
	c.fgCancel = nil
	if c.closed {
		c.removeInputLocked(in)
		return nil
	}
	seq := in.entry.Seq
	if ctx.Err() != nil {
		// The interrupt ends this input and the older ones it would have
		// carried as context; each gets its marker.
		var batch []Entry
		for _, r := range append(c.olderInputsLocked(in), in) {
			c.removeInputLocked(r)
			batch = append(batch, markerEntry(MarkerInterrupted, r.entry.Seq,
				fmt.Sprintf("#%d was interrupted before an owner was chosen", r.entry.Seq)))
		}
		c.commitLocked("", batch, nil)
		c.maybeStartCycleLocked()
		return c.parts[c.owner]
	}
	d := *dec
	c.enqueueLocked(&Event{Kind: EventRoute, Participant: d.Target, Epoch: c.epoch, Route: &d})
	if err != nil {
		// The older inputs stay queued: one of them may still find an owner.
		c.parkLocked(in)
		c.commitLocked("", []Entry{markerEntry(MarkerNoOwner, seq,
			fmt.Sprintf("no participant may take #%d: %v", seq, err))}, nil)
		c.maybeStartCycleLocked()
		return c.parts[c.owner]
	}
	ps := c.parts[dec.Target]
	if dec.Target != c.owner {
		// Based on the input, so recovery knows which request the new
		// owner was answering.
		oc := Entry{
			Kind:    KindOwnerChange,
			BasedOn: seq,
			Owner:   &OwnerChange{From: c.owner, To: dec.Target, Reason: ReasonRouter},
		}
		if c.commitLocked("", []Entry{oc}, &Guard{Owner: c.owner, Epoch: c.epoch}) == nil {
			c.parkLocked(in)
			c.maybeStartCycleLocked()
			return c.parts[c.owner]
		}
	}
	c.removeInputLocked(in)
	carried := c.dropContextInputsLocked(in)
	a := c.newActivationLocked(ps, actUser, &in.entry, true, 0)
	a.cycleInput = seq
	a.steered = carried
	c.startForegroundLocked(a)
	return nil
}

// reconcile checks the entries addressed to an idle participant past its
// cursor. The cursor is read without the mutex.
func (c *Coordinator) reconcile(ps *partState) {
	cur := ps.p.State().Cursor
	c.mu.Lock()
	c.checkAddressedLocked(ps, cur)
	c.mu.Unlock()
}

// routeRequest gathers what the router may use. It calls participants and
// reads the store without the mutex.
func (c *Coordinator) routeRequest(ctx context.Context, in *inputRec) (RouteRequest, map[string]bool) {
	c.mu.Lock()
	owner, last := c.owner, c.lastSeq
	c.mu.Unlock()
	req := RouteRequest{
		OperationID: in.q.OperationID,
		Query:       in.entry.Text(),
		Owner:       owner,
		Explicit:    in.q.Explicit,
		NeedsGroups: slices.Clone(in.q.NeedsGroups),
	}
	avail := make(map[string]bool, len(c.order))
	for _, ps := range c.order {
		ok := c.available(ps.name)
		avail[ps.name] = ok
		lag, _ := c.countAfter(ctx, ps.p.State().Cursor, false)
		req.Participants = append(req.Participants, RouteCandidate{Descriptor: cloneDescriptor(&ps.desc), Available: ok, Lag: uint64(lag)})
	}
	if edge := c.parts[c.edge]; edge != nil {
		if s := c.newestSummary(ctx, edge.name); s != nil {
			req.PeerSummary = s.Text()
			n, _ := c.countAfter(ctx, s.Coverage.Through, true)
			req.SummaryStale = n > c.cfg.Import.StaleAfter
		}
	}
	var from uint64
	if last > recentEntries {
		from = last - recentEntries
	}
	recent, err := c.store.Read(ctx, from, Filter{}, recentEntries)
	if err != nil {
		agentscope.Log().WithError(err).Warn("thread: read recent entries for routing")
	}
	req.Recent = recent
	return req, avail
}

// countAfter counts the entries after seq, leaving out summaries when asked.
func (c *Coordinator) countAfter(ctx context.Context, after uint64, skipSummaries bool) (int, error) {
	entries, err := c.store.Read(ctx, after, Filter{}, 0)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range entries {
		if skipSummaries && entries[i].Kind == KindSummary {
			continue
		}
		n++
	}
	return n, nil
}

// newestSummary returns the newest summary written for target, nil if none.
func (c *Coordinator) newestSummary(ctx context.Context, target string) *Entry {
	entries, err := c.store.Read(ctx, 0, Filter{Kinds: []Kind{KindSummary}}, 0)
	if err != nil {
		agentscope.Log().WithError(err).Warn("thread: read summaries")
		return nil
	}
	return newestSummaryFor(entries, target)
}

// newestSummaryFor returns the newest summary entry for target in entries.
func newestSummaryFor(entries []Entry, target string) *Entry {
	for i := len(entries) - 1; i >= 0; i-- {
		e := &entries[i]
		if e.Kind == KindSummary && e.Summary != nil && e.Summary.Target == target && e.Coverage != nil {
			return e
		}
	}
	return nil
}

// route asks the router and checks its answer. A router error other than
// ErrNoCandidate or ErrNotAllowed, or a target that is unknown or
// unavailable, falls back to the default rule. The error is non-nil only
// when no participant may take the query or ctx ended.
func (c *Coordinator) route(ctx context.Context, req *RouteRequest, avail map[string]bool) (RouteDecision, error) {
	if c.cfg.Router == nil {
		return c.defaultRoute(req, avail)
	}
	dec, err := c.cfg.Router.Route(ctx, *req)
	switch {
	case err == nil:
		if c.validTarget(dec.Target, avail) {
			return dec, nil
		}
		agentscope.Log().WithField("target", dec.Target).Warn("thread: router chose an unknown or unavailable participant; using the default rule")
	case errors.Is(err, ErrNoCandidate), errors.Is(err, ErrNotAllowed):
		return dec, err
	case ctx.Err() != nil:
		return dec, ctx.Err()
	default:
		agentscope.Log().WithError(err).Warn("thread: router failed; using the default rule")
	}
	fb, ferr := c.defaultRoute(req, avail)
	fb.ClassifierCalls, fb.ClassifierUsage, fb.UsageKnown = dec.ClassifierCalls, dec.ClassifierUsage, dec.UsageKnown
	return fb, ferr
}

// validTarget reports whether name may own the query now: it is registered
// and available.
func (c *Coordinator) validTarget(name string, avail map[string]bool) bool {
	return c.parts[name] != nil && avail[name]
}

// defaultRoute is the rule without a router: the participant the user named
// when it is available, else the owner when it is, else the first available
// participant.
func (c *Coordinator) defaultRoute(req *RouteRequest, avail map[string]bool) (RouteDecision, error) {
	if req.Explicit != "" {
		if c.validTarget(req.Explicit, avail) {
			return RouteDecision{Target: req.Explicit, Source: RouteExplicit, Reason: "named by the user", Confidence: 1}, nil
		}
		return RouteDecision{}, fmt.Errorf("%w: %q", ErrNotAllowed, req.Explicit)
	}
	if req.Owner != "" && c.validTarget(req.Owner, avail) {
		return RouteDecision{Target: req.Owner, Source: RouteDefault, Reason: "stay with the owner", Confidence: 1}, nil
	}
	for _, ps := range c.order {
		if c.validTarget(ps.name, avail) {
			return RouteDecision{Target: ps.name, Source: RouteDefault, Reason: "first available participant", Confidence: 1}, nil
		}
	}
	return RouteDecision{}, ErrNoCandidate
}

// recordRouterUsage records classifier calls in the ledger: one entry with
// the summed usage when it is known, otherwise one entry with nil usage per
// call.
func (c *Coordinator) recordRouterUsage(dec *RouteDecision) {
	if dec.ClassifierCalls <= 0 {
		return
	}
	now := c.cfg.Clock()
	if dec.UsageKnown && dec.ClassifierUsage != nil {
		c.ledger.Record(LedgerEntry{Participant: "router", Kind: UsageClassifier, Usage: dec.ClassifierUsage, Time: now})
		return
	}
	for range dec.ClassifierCalls {
		c.ledger.Record(LedgerEntry{Participant: "router", Kind: UsageClassifier, Time: now})
	}
}
