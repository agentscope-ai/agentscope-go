package thread

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Reserved names. User input carries Origin{Participant: userName}.
const (
	userName        = "user"
	coordinatorName = "coordinator"
)

// DefaultPeerPrompt asks a cloud fork for a summary another participant can
// continue from. {target}, {window} and {budget} are replaced.
const DefaultPeerPrompt = "Write a summary of this conversation for {target}, another assistant that runs on the user's device " +
	"with a {window}-token context window and will continue the work with the user. Stay under {budget} tokens. " +
	"Include the user's goals and constraints, decisions made, the current state of the work, open items, and the " +
	"file paths, names and numbers it will need. Leave out tool output dumps. Plain text, no preamble."

// DefaultOwnPrompt asks a fork for a summary its own participant continues
// from after its earlier messages are removed.
const DefaultOwnPrompt = "Write notes on this conversation so you can continue it after the earlier messages are removed. " +
	"Use these sections, each a short list: Task (what the user wants and the constraints), Current state (what is done and what is in progress), " +
	"Files and identifiers (paths, names and numbers you will need), Decisions (what was chosen and why), Errors and fixes, Next steps. " +
	"Keep every detail you would need to continue. Plain text, no preamble."

// DefaultOwnUpdatePrompt asks a fork to update the participant's notes with
// edits; the notes follow it.
const DefaultOwnUpdatePrompt = "Update your notes below with what happened in this conversation since they were written. " +
	"Answer with edits only, each in this form:\n" + editFind + "\ntext copied exactly from the notes\n" + editSep + "\nits replacement\n" + editReplace + "\n" +
	"An empty FIND adds the replacement at the end. Keep what still matters and drop what no longer does; leave unchanged parts out. " +
	"To rewrite a section, replace it whole. If nothing needs to change, answer " + noteNoChange + "."

// Defaults for zero Config fields.
const (
	defaultPeerEvery        = 20000
	defaultPeerRatio        = 0.8
	defaultPeerBudget       = 1024
	defaultOwnInit          = 10000
	defaultOwnEvery         = 5000
	defaultOwnMaxTokens     = 8192
	defaultMaxRewrites      = 1
	defaultEdgeShare        = 0.5
	defaultMaxRounds        = 8
	defaultStaleAfter       = 40
	defaultMaxWakes         = 4
	defaultMaxAssignDepth   = 2
	defaultMaxPendingInputs = 16
	defaultEventBuffer      = 256
	// recentEntries is how many of the newest entries a RouteRequest carries.
	recentEntries = 30
)

// Query is one user message for the Coordinator.
type Query struct {
	// OperationID identifies the query; one is generated when empty. The
	// user_input entry's dedup key is "user:"+OperationID, so a retried
	// Submit with the same ID appends nothing and starts nothing.
	OperationID string
	Text        string
	// Explicit names a participant the user addressed, "" if none.
	Explicit    string
	NeedsGroups []string
}

// SummaryPolicy schedules the summaries cloud participants fork from their
// last request.
type SummaryPolicy struct {
	// PeerTarget is the participant peer summaries are written for; default
	// the only edge participant.
	PeerTarget string
	// PeerRatio is the share of the target's window that its newest peer
	// summary and the entries after it, what a handover would bring it, may
	// fill; default 0.8, a negative value turns the rule off. A writer
	// refreshes the summary at the end of a reply when that load, plus what
	// the reply added to it, reaches the share, so the target can take over
	// (also by failover, when the cloud cannot fork) with what it holds. The
	// target's window is the one its State reports, else its Descriptor's;
	// when neither is known, PeerEvery's default applies instead.
	PeerRatio float64
	// PeerEvery is the growth, in tokens of the writer's input, between peer
	// summaries. Zero (the default) leaves the refresh to PeerRatio, or uses
	// 20000 when the target's window is unknown; a positive value applies
	// as well as PeerRatio.
	PeerEvery int
	// PeerPrompt defaults to DefaultPeerPrompt; {target}, {window} and
	// {budget} are replaced.
	PeerPrompt string
	// PeerMaxTokens defaults to twice the target's SummaryBudget, at least 512.
	PeerMaxTokens int
	// OwnInit and OwnEvery schedule a cloud participant's own notes the way
	// Claude Code schedules its session memory: the first notes once its
	// input reaches OwnInit (default 10000 tokens), then an update whenever
	// the input grew OwnEvery (default 5000) since the last one, checked at
	// the end of each reply. An update edits the previous notes
	// (OwnUpdatePrompt). The notes are not applied when written: the
	// participant holds them and installs them, without a model call, when
	// its context reaches its compaction line (Forker.OfferOwnSummary).
	OwnInit  int
	OwnEvery int
	// OwnPrompt asks for the first notes; it defaults to DefaultOwnPrompt.
	OwnPrompt string
	// OwnUpdatePrompt asks for edits of the notes, which follow it; it
	// defaults to DefaultOwnUpdatePrompt.
	OwnUpdatePrompt string
	// OwnMaxTokens caps an own fork's output and the notes' size; default
	// 8192.
	OwnMaxTokens int
	// MaxRewrites bounds a fork's rewrites after its validation fails;
	// 0 takes the default 1, a negative value allows none.
	MaxRewrites int
	// Disabled turns summary forks off.
	Disabled bool
}

// ImportPolicy bounds what the Coordinator imports into a participant
// before it runs.
type ImportPolicy struct {
	// EdgeShare is the share of an edge window an import may fill; default
	// 0.5. Only edge imports are budgeted: a cloud participant imports what
	// it lacks in full, and what outgrows its window is compacted there.
	EdgeShare float64
	// MaxRounds bounds the chunk and compact rounds of an edge import that
	// does not fit (the local fallback when no new summary covers the
	// entries); default 8.
	MaxRounds int
	// StaleAfter marks a peer summary stale for routing when more than this
	// many readable entries follow it; default 40.
	StaleAfter int
}

// Limits bounds the Coordinator's work.
type Limits struct {
	// MaxWakesPerQuery bounds the wakes of an idle owner per query cycle;
	// default 4.
	MaxWakesPerQuery int
	// MaxAssignDepth bounds nested background tasks; default 2.
	MaxAssignDepth int
	// MaxPendingInputs bounds the queued user inputs still waiting for a query
	// cycle; default 16. Inputs parked after a no_owner (or recovered) marker
	// do not count: nothing runs for them until a later cycle carries them.
	// At most MaxPendingInputs of those are kept; the oldest beyond that is
	// retired, and stays in the log as context.
	MaxPendingInputs int
	// EventBuffer is the user event channel's buffer; default 256.
	EventBuffer int
}

// Config configures a Coordinator.
type Config struct {
	Store        Store
	Participants []Participant
	// Router picks the owner of each query. Nil stays with the owner when it
	// may take the query, else picks the first available participant that may
	// (a participant the query names explicitly comes first).
	Router Router
	// Ledger defaults to NewLedger(nil).
	Ledger *Ledger
	// Events defaults to NewEventList(EventListOptions{}).
	Events *EventList
	// Leases, when set, are released with ReleaseAll(participant) after each
	// activation.
	Leases    *Leases
	Summaries SummaryPolicy
	Import    ImportPolicy
	Limits    Limits
	// Available reports whether a participant can run now; nil means all are.
	Available func(name string) bool
	// Clock stamps entries and ledger records; nil uses time.Now.
	Clock func() time.Time
	// RecoveryNote is the host's account of why the previous process
	// stopped, from its crash log say. When recovery resumes a request the
	// stopped process was answering, the restarted marker records it (up to
	// 1,000 characters) and the resuming participant's input quotes it on one
	// line, cut at 200 characters. Empty says nothing beyond the log.
	RecoveryNote string
}

// EventKind names what an Event carries.
type EventKind string

const (
	// EventAgent carries an agent event of the foreground activation, or a
	// confirmation request of any activation.
	EventAgent EventKind = "agent"
	// EventEntry carries an owner_change, marker, assign, report, summary or
	// reply entry as it is committed.
	EventEntry EventKind = "entry"
	// EventRoute carries a routing decision.
	EventRoute EventKind = "route"
	// EventIdle says no activation, queued work or fork job remains.
	EventIdle EventKind = "idle"
	// EventError carries a failure the user may want to see.
	EventError EventKind = "error"
)

// Event is what the host shows the user.
//
// Only items with Foreground set are the owner's reply. A host that renders
// the reply must render only those (plus the confirmation requests it
// answers): late output of a participant that lost the foreground and
// background reports arrive as EventEntry items with Foreground false, and
// showing them as the reply would break invariant 4.
type Event struct {
	Kind        EventKind
	Participant string
	Path        string
	Epoch       uint64
	// Foreground is set for events of the activation holding the foreground,
	// for the events it showed before a Transfer that the Coordinator
	// forwards after the commit (TransferRequest.EventsBefore), and for
	// reply entries.
	Foreground bool
	Agent      event.Event    // EventAgent
	Entry      *Entry         // EventEntry
	Route      *RouteDecision // EventRoute
	Err        error          // EventError
}

// Coordinator is the single writer of a Main Thread. It routes queries, owns
// the foreground owner and its epoch, forwards the owner's output, commits
// the participants' Transfer, Assign, Report and Publish, runs background
// tasks, wakes participants, imports entries before each activation and
// schedules summary forks. It is safe for concurrent use.
//
// One mutex guards its state. It is never held while calling a participant,
// the router or the Available callback, nor while sending to the Events
// channel. Every log append happens under it, so a
// guard read and its append are one step.
type Coordinator struct {
	cfg    Config
	store  Store
	ledger *Ledger
	events *EventList
	runID  string // makes activation and job IDs unique across restarts
	edge   string // the peer summary target, "" when there is none

	parts map[string]*partState // fixed after construction
	order []*partState          // in Config order

	ctx    context.Context // canceled by Close
	cancel context.CancelFunc
	bg     context.Context // never canceled: sealing appends survive Close
	wg     sync.WaitGroup

	out       chan Event
	outSignal chan struct{}

	mu       sync.Mutex
	closed   bool
	owner    string
	epoch    uint64
	lastSeq  uint64
	inputs   []*inputRec // undelivered user inputs, oldest first
	cycle    cycleState
	fg       *activation // the foreground activation, queued or running
	routing  bool        // a query cycle is choosing an owner
	fgCancel context.CancelFunc
	outbox   []*Event
	space    chan struct{} // closed and replaced whenever the outbox shrinks
	// open is the transferred activation whose backlog is open, nil when
	// none is: entries committed meanwhile wait in its held list, so entry
	// events stay in log order.
	open     *activation
	work     int
	idle     chan struct{} // closed while work is zero
	nextAct  int
	nextTask int
	nextJob  int
	tasks    map[uint64]*taskRec // by assign seq
}

// partState is the Coordinator's view of one participant. Fields below the
// first group are guarded by Coordinator.mu.
type partState struct {
	name   string
	desc   Descriptor
	p      Participant
	forker Forker // set for cloud participants implementing Forker
	// host is the handle bound to p; Close releases it. Written only by
	// NewCoordinator and Close.
	host *coordHost

	running *activation
	queue   []*activation
	// seen is the highest seq the Coordinator delivered (trigger, import or
	// Pending); it backs up the participant's cursor.
	seen         uint64
	lastActivity uint64 // highest seq of an entry it wrote
	// lastSealed is the seq of its last reply, transfer owner_change or
	// task report: the newest of its own outputs that a summary fork covers.
	lastSealed   uint64
	heldCoverage *Coverage // coverage of the peer summary it holds
	resetFrom    uint64    // seq of the summary its last clear applied
	// notify holds, by seq, the Notify reports addressed to it that no
	// foreground activation of it has received yet. They wake it while it is
	// the idle owner.
	notify map[uint64]*Entry

	// restored is set by recovery and stays set for the process: the
	// participant's checkpoint may predate what it wrote, so its imports also
	// bring its own entries based past its cursor. Keeping it set after the
	// first import can re-import a few own entries; clearing it early would
	// lose them when that first activation never applied its Sync.
	restored bool

	forking   bool
	growth    int
	lastInput int
	// ownNotes are the participant's newest own notes; ownAt is its input
	// when the last own fork started.
	ownNotes string
	ownAt    int
	// peerLoad is the peer target's handover load (newest peer summary and
	// the entries after it) this writer measured at the end of its last
	// reply; the next measure's rise predicts the reply after it.
	peerLoad int
}

// inputRec is an undelivered user input.
type inputRec struct {
	entry  Entry
	q      Query
	parked bool // waits for the next Submit (no owner, or recovered)
}

// cycleState holds the counters of the current query cycle.
type cycleState struct {
	input      uint64
	text       string
	wakes      int
	failedOver bool
	// progress is the newest progress entry an activation answering this
	// cycle wrote; a failover points its successor at it.
	progress *Entry
	// noWake holds the reports whose wake failed in this cycle; they wait
	// for the owner's next foreground activation instead of waking it again.
	noWake map[uint64]bool
}

// taskRec is one background task.
type taskRec struct {
	seq      uint64
	id       string
	assigner string
	// assignerAct is the ID of the activation that assigned it, "" when it
	// was assigned outside an activation. A background activation receives
	// through Pending only the reports of tasks it assigned itself.
	assignerAct string
	assignee    string
	notify      bool
	depth       int // depth of the activation that runs it
	queued      bool
}

// NewCoordinator validates cfg, binds every participant to its own Host
// handle, reads the log head and runs recovery. ctx bounds construction
// only; background work runs until Close. Close does not close the Store.
func NewCoordinator(ctx context.Context, cfg Config) (*Coordinator, error) { //nolint:gocritic // value parameter keeps the documented API
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	c := &Coordinator{
		cfg:       cfg,
		store:     cfg.Store,
		ledger:    cfg.Ledger,
		events:    cfg.Events,
		runID:     agentscope.GenerateID(),
		edge:      cfg.Summaries.PeerTarget,
		parts:     make(map[string]*partState, len(cfg.Participants)),
		out:       make(chan Event, cfg.Limits.EventBuffer),
		outSignal: make(chan struct{}, 1),
		space:     make(chan struct{}),
		idle:      closedChan(),
		tasks:     make(map[uint64]*taskRec),
	}
	c.ctx, c.cancel = context.WithCancel(context.WithoutCancel(ctx))
	c.bg = context.WithoutCancel(ctx)
	for _, p := range cfg.Participants {
		d := p.Descriptor()
		ps := &partState{name: d.Name, desc: cloneDescriptor(&d), p: p}
		if f, ok := p.(Forker); ok && d.Zone == ZoneCloud {
			ps.forker = f
		}
		c.parts[d.Name] = ps
		c.order = append(c.order, ps)
	}
	for _, ps := range c.order {
		h := &coordHost{c: c, ps: ps}
		if err := ps.p.Bind(h); err != nil {
			c.cancel()
			c.releaseAll()
			return nil, fmt.Errorf("thread: bind %s: %w", ps.name, err)
		}
		ps.host = h
	}
	if err := c.recover(ctx); err != nil {
		c.cancel()
		c.releaseAll()
		return nil, err
	}
	c.wg.Add(1)
	go c.dispatch()
	return c, nil
}

// normalize validates the config and fills its defaults.
func (cfg *Config) normalize() error {
	if cfg.Store == nil {
		return errors.New("thread: coordinator needs a store")
	}
	if len(cfg.Participants) == 0 {
		return errors.New("thread: coordinator needs at least one participant")
	}
	names := make(map[string]Zone, len(cfg.Participants))
	var edges []string
	for i, p := range cfg.Participants {
		if p == nil {
			return fmt.Errorf("thread: participant %d is nil", i)
		}
		d := p.Descriptor()
		if err := d.Validate(); err != nil {
			return err
		}
		if d.Name == userName || d.Name == coordinatorName {
			return fmt.Errorf("thread: participant name %q is reserved", d.Name)
		}
		if _, dup := names[d.Name]; dup {
			return fmt.Errorf("thread: participant name %q is used twice", d.Name)
		}
		names[d.Name] = d.Zone
		if d.Zone == ZoneEdge {
			edges = append(edges, d.Name)
		}
	}
	s := &cfg.Summaries
	switch {
	case s.PeerTarget != "":
		z, ok := names[s.PeerTarget]
		if !ok {
			return fmt.Errorf("thread: summary peer target %q is not a participant", s.PeerTarget)
		}
		if z != ZoneEdge {
			return fmt.Errorf("thread: summary peer target %q is not an edge participant", s.PeerTarget)
		}
	case len(edges) > 1:
		return fmt.Errorf("thread: %d edge participants; set SummaryPolicy.PeerTarget to the one summaries are written for", len(edges))
	case len(edges) == 1:
		s.PeerTarget = edges[0]
	}
	if s.PeerEvery < 0 {
		s.PeerEvery = 0
	}
	if s.PeerRatio == 0 {
		s.PeerRatio = defaultPeerRatio
	}
	if s.PeerPrompt == "" {
		s.PeerPrompt = DefaultPeerPrompt
	}
	if s.OwnInit <= 0 {
		s.OwnInit = defaultOwnInit
	}
	if s.OwnEvery <= 0 {
		s.OwnEvery = defaultOwnEvery
	}
	if s.OwnPrompt == "" {
		s.OwnPrompt = DefaultOwnPrompt
	}
	if s.OwnUpdatePrompt == "" {
		s.OwnUpdatePrompt = DefaultOwnUpdatePrompt
	}
	if s.OwnMaxTokens <= 0 {
		s.OwnMaxTokens = defaultOwnMaxTokens
	}
	switch {
	case s.MaxRewrites == 0:
		s.MaxRewrites = defaultMaxRewrites
	case s.MaxRewrites < 0:
		s.MaxRewrites = 0
	}
	im := &cfg.Import
	im.EdgeShare = shareOrDefault(im.EdgeShare, defaultEdgeShare)
	if im.EdgeShare > 1 || s.PeerRatio > 1 {
		return errors.New("thread: the edge import share and the peer summary ratio must be at most 1")
	}
	im.MaxRounds = intOrDefault(im.MaxRounds, defaultMaxRounds)
	im.StaleAfter = intOrDefault(im.StaleAfter, defaultStaleAfter)
	l := &cfg.Limits
	l.MaxWakesPerQuery = intOrDefault(l.MaxWakesPerQuery, defaultMaxWakes)
	l.MaxAssignDepth = intOrDefault(l.MaxAssignDepth, defaultMaxAssignDepth)
	l.MaxPendingInputs = intOrDefault(l.MaxPendingInputs, defaultMaxPendingInputs)
	l.EventBuffer = intOrDefault(l.EventBuffer, defaultEventBuffer)
	if cfg.Ledger == nil {
		cfg.Ledger = NewLedger(nil)
	}
	if cfg.Events == nil {
		cfg.Events = NewEventList(EventListOptions{})
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return nil
}

func shareOrDefault(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

func intOrDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func cloneDescriptor(d *Descriptor) Descriptor {
	c := *d
	c.ToolGroups = slices.Clone(d.ToolGroups)
	return c
}

// Events returns the channel of user-facing events. It must be drained:
// agent events wait for room (backpressure) until Close, which closes it.
// Show only items with Foreground set as the owner's reply (see Event).
func (c *Coordinator) Events() <-chan Event { return c.out }

// Ledger returns the shared ledger.
func (c *Coordinator) Ledger() *Ledger { return c.ledger }

// Owner returns the current owner and epoch.
func (c *Coordinator) Owner() (string, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owner, c.epoch
}

// Submit appends a user_input and returns its seq; the work proceeds in the
// background. While a foreground activation runs, the input steers it: the
// owner receives it at its next boundary, and an input still undelivered
// when the activation ends starts a new query cycle. Submit fails, appending
// nothing, when MaxPendingInputs inputs are already waiting.
func (c *Coordinator) Submit(ctx context.Context, q Query) (uint64, error) { //nolint:gocritic // value parameter keeps the documented API
	if strings.TrimSpace(q.Text) == "" {
		return 0, errors.New("thread: query text is empty")
	}
	if q.OperationID == "" {
		q.OperationID = agentscope.GenerateID()
	}
	q.NeedsGroups = slices.Clone(q.NeedsGroups)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, ErrClosed
	}
	if n := c.waitingInputsLocked(); n >= c.cfg.Limits.MaxPendingInputs {
		return 0, fmt.Errorf("thread: %d user inputs are already waiting; try again when the current work ends", n)
	}
	e := Entry{
		Kind:     KindUserInput,
		Origin:   Origin{Participant: userName},
		Content:  textContent(q.Text),
		DedupKey: "user:" + q.OperationID,
		Time:     c.cfg.Clock(),
	}
	seq, err := c.store.Append(ctx, []Entry{e}, nil)
	if err != nil {
		return 0, fmt.Errorf("thread: append user input: %w", err)
	}
	if seq <= c.lastSeq {
		// A retry of a Submit already in the log.
		return seq, nil
	}
	stored, err := c.store.Read(c.bg, seq-1, Filter{}, 1)
	if err != nil {
		return 0, fmt.Errorf("thread: read back user input #%d: %w", seq, err)
	}
	if len(stored) != 1 {
		return 0, fmt.Errorf("thread: user input #%d is missing from the log", seq)
	}
	c.noteAppendedLocked(stored)
	c.inputs = append(c.inputs, &inputRec{entry: stored[0], q: q})
	c.maybeStartCycleLocked()
	return seq, nil
}

// SubmitUserConfirm answers a confirmation request a participant's
// activation emitted.
func (c *Coordinator) SubmitUserConfirm(participant, path string, r *event.UserConfirmResultEvent) error {
	ps, ok := c.parts[participant]
	if !ok {
		return fmt.Errorf("thread: unknown participant %q", participant)
	}
	if r == nil {
		return errors.New("thread: confirmation result is nil")
	}
	return ps.p.SubmitUserConfirm(path, r)
}

// Interrupt cancels the foreground activation, or the routing of the query
// that would start it.
func (c *Coordinator) Interrupt() {
	c.mu.Lock()
	cancel := c.fgCancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// WaitIdle waits until no activation, queued work or fork job remains.
func (c *Coordinator) WaitIdle(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	ch := c.idle
	c.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return ErrClosed
	}
}

// Close cancels every activation, routing and fork job, waits for their
// goroutines, releases the participants that implement Releaser and closes
// the Events channel. Activations still seal what they showed. It does not
// close the Store. A second Close returns ErrClosed.
func (c *Coordinator) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	c.wg.Wait()
	c.releaseAll()
	close(c.out)
	return nil
}

// releaseAll hands every bound participant that implements Releaser its
// Host back, so another Coordinator can bind it. Callers make sure no
// activation or fork job of this Coordinator still runs.
func (c *Coordinator) releaseAll() {
	for _, ps := range c.order {
		if ps.host == nil {
			continue
		}
		if r, ok := ps.p.(Releaser); ok {
			r.Release(ps.host)
		}
		ps.host = nil
	}
}

// ---------------------------------------------------------------- appends

// appendLocked writes entries as one batch under the guard and returns the
// stored copies. Holding the mutex makes the guard read and the append one
// step. A batch the store recognized as a retry returns no entries.
func (c *Coordinator) appendLocked(entries []Entry, g *Guard) ([]Entry, error) {
	now := c.cfg.Clock()
	for i := range entries {
		if entries[i].Time.IsZero() {
			entries[i].Time = now
		}
	}
	last, err := c.store.Append(c.bg, entries, g)
	if err != nil {
		return nil, err
	}
	if last <= c.lastSeq {
		return nil, nil
	}
	first := last - uint64(len(entries)) + 1
	stored, err := c.store.Read(c.bg, first-1, Filter{}, len(entries))
	if err != nil {
		return nil, fmt.Errorf("thread: read back #%d–#%d: %w", first, last, err)
	}
	c.noteAppendedLocked(stored)
	return stored, nil
}

// noteAppendedLocked mirrors the head and each writer's last activity.
func (c *Coordinator) noteAppendedLocked(stored []Entry) {
	for i := range stored {
		e := &stored[i]
		if e.Kind == KindOwnerChange && e.Owner != nil {
			c.owner = e.Owner.To
		}
		c.epoch = e.Epoch
		c.lastSeq = max(c.lastSeq, e.Seq)
		if ps := c.parts[e.Origin.Participant]; ps != nil {
			ps.lastActivity = max(ps.lastActivity, e.Seq)
		}
	}
}

// commitLocked appends and emits a batch; a failure becomes an error event.
func (c *Coordinator) commitLocked(who string, entries []Entry, g *Guard) []Entry {
	stored, err := c.appendLocked(entries, g)
	if err != nil {
		c.failLocked(who, fmt.Errorf("thread: append: %w", err))
		return nil
	}
	c.emitEntriesLocked(stored)
	return stored
}

// markerEntry builds a coordinator marker based on seq.
func markerEntry(kind MarkerKind, basedOn uint64, detail string) Entry {
	return Entry{
		Kind:    KindMarker,
		BasedOn: basedOn,
		Marker:  &Marker{Kind: kind, Detail: detail},
	}
}

func textContent(s string) []message.ContentBlock {
	if s == "" {
		return nil
	}
	return []message.ContentBlock{message.TextBlock{Type: string(message.ContentBlockText), Text: s}}
}

func cloneDigest(d *ToolDigest) *ToolDigest {
	if d == nil || len(d.Calls) == 0 {
		return nil
	}
	return &ToolDigest{Calls: slices.Clone(d.Calls)}
}

func cloneCoverage(cv *Coverage) *Coverage {
	if cv == nil {
		return nil
	}
	c := *cv
	c.Excluded = slices.Clone(cv.Excluded)
	return &c
}

// ---------------------------------------------------------------- events

// emitEntriesLocked queues an EventEntry for each entry the user is shown.
func (c *Coordinator) emitEntriesLocked(entries []Entry) {
	if a := c.open; a != nil {
		// A Transfer's entries wait for the events its activation showed
		// before it; later entries wait behind them.
		a.held = append(a.held, entries...)
		return
	}
	for i := range entries {
		e := &entries[i]
		switch e.Kind {
		case KindOwnerChange, KindMarker, KindAssign, KindReport, KindSummary, KindReply:
		default:
			continue
		}
		cp := cloneEntry(e)
		c.enqueueLocked(&Event{
			Kind:        EventEntry,
			Participant: e.Origin.Participant,
			Path:        e.Origin.Path,
			Epoch:       e.Epoch,
			Foreground:  e.Kind == KindReply,
			Entry:       &cp,
		})
	}
}

// failLocked logs err and queues it as an error event.
func (c *Coordinator) failLocked(who string, err error) {
	agentscope.Log().WithError(err).WithField("participant", who).Warn("thread: coordinator")
	c.enqueueLocked(&Event{Kind: EventError, Participant: who, Epoch: c.epoch, Err: err})
}

// enqueueLocked adds ev to the outbox. Entry, route, idle and error events
// are queued unconditionally; entry events come in log order (while a
// Transfer's backlog is open, emitEntriesLocked holds them back behind the
// Transfer's own entries). Agent events go through forwardAgent, which waits
// for room first.
func (c *Coordinator) enqueueLocked(ev *Event) {
	c.outbox = append(c.outbox, ev)
	select {
	case c.outSignal <- struct{}{}:
	default:
	}
}

// dispatch moves events from the outbox to the Events channel until Close.
func (c *Coordinator) dispatch() {
	defer c.wg.Done()
	for {
		c.mu.Lock()
		if len(c.outbox) == 0 {
			c.mu.Unlock()
			select {
			case <-c.outSignal:
				continue
			case <-c.ctx.Done():
				return
			}
		}
		ev := c.outbox[0]
		c.outbox[0] = nil
		c.outbox = c.outbox[1:]
		close(c.space)
		c.space = make(chan struct{})
		c.mu.Unlock()
		select {
		case c.out <- *ev:
		case <-c.ctx.Done():
			return
		}
	}
}

// ---------------------------------------------------------------- work

// addWorkLocked counts one unit of pending work: a routing cycle, a queued
// or running activation, or a fork job.
func (c *Coordinator) addWorkLocked() {
	if c.work == 0 {
		c.idle = make(chan struct{})
	}
	c.work++
}

// doneWorkLocked ends one unit of work; at zero it signals idleness.
func (c *Coordinator) doneWorkLocked() {
	c.work--
	if c.work == 0 {
		close(c.idle)
		c.enqueueLocked(&Event{Kind: EventIdle, Participant: c.owner, Epoch: c.epoch})
	}
}

// available asks the host whether a participant can run. Never call it
// with the mutex held.
func (c *Coordinator) available(name string) bool {
	if c.cfg.Available == nil {
		return true
	}
	return c.cfg.Available(name)
}

// availability snapshots Available for every participant.
func (c *Coordinator) availability() map[string]bool {
	out := make(map[string]bool, len(c.order))
	for _, ps := range c.order {
		out[ps.name] = c.available(ps.name)
	}
	return out
}
