package thread

import (
	"context"
	"errors"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// This file holds the contracts between the Coordinator, the participants that
// wrap agents (package thread/participant) and the router (package
// thread/route). The thread package itself never imports the agent package.

// ---------------------------------------------------------------- routing

// RouteCandidate is one participant as the router sees it.
type RouteCandidate struct {
	Descriptor Descriptor
	Available  bool
	// Lag is the number of entries it has not imported yet.
	Lag uint64
}

// RouteRequest is everything a router may use to pick the owner of a query.
type RouteRequest struct {
	OperationID  string // one per query; reused on retries
	Query        string
	Owner        string // current owner, "" at the start
	Explicit     string // a participant the user named, "" if none
	NeedsGroups  []string
	Participants []RouteCandidate
	PeerSummary  string // newest summary written for the edge window, "" if none
	SummaryStale bool
	// Recent are the newest log entries, newest last.
	Recent []Entry
}

// RouteSource says which step of the decision order decided.
type RouteSource string

const (
	RouteExplicit   RouteSource = "explicit"
	RouteHard       RouteSource = "hard"
	RouteRule       RouteSource = "rule"
	RouteClassifier RouteSource = "classifier"
	RouteDefault    RouteSource = "default"
)

// RouteDecision is a router's answer.
type RouteDecision struct {
	Target     string
	Source     RouteSource
	Reason     string
	Confidence float64
	// Classifier accounting, when a classifier was called (even if its answer
	// was not used). UsageKnown is false when any attempt's usage is unknown.
	ClassifierCalls int
	ClassifierUsage *model.ChatUsage
	UsageKnown      bool
}

// Router picks the owner of a query. Implementations: package thread/route.
type Router interface {
	Route(ctx context.Context, req RouteRequest) (RouteDecision, error)
}

var (
	// ErrNoCandidate means no available participant may take the query.
	ErrNoCandidate = errors.New("thread: no participant may take this query")
	// ErrNotAllowed means the query named a participant it may not go to.
	ErrNotAllowed = errors.New("thread: the named participant may not take this query")
)

// ---------------------------------------------------------------- participants

// Sync is one atomic change to a participant's imported thread state. It maps
// one to one onto agent.ThreadUpdate.
type Sync struct {
	// Reset clears the participant's context and own summary first (the clear
	// rule at an edge takeover). Between activations only.
	Reset bool
	// PeerSummary, when non-nil, is a summary entry written for this
	// participant's window. Imported messages it covers are dropped.
	PeerSummary *Entry
	// Messages are projected entries, each carrying SeqMetadataKey.
	Messages []*message.Msg
	// Cursor is the new cursor; zero keeps it. It never moves back.
	Cursor uint64
}

// Empty reports whether s changes nothing.
func (s *Sync) Empty() bool {
	return s == nil || (!s.Reset && s.PeerSummary == nil && len(s.Messages) == 0 && s.Cursor == 0)
}

// ParticipantState is a participant's current view of itself.
type ParticipantState struct {
	Cursor      uint64
	InputTokens int // estimated tokens of its next model input
	Window      int // resolved context window, 0 when unknown
	// SummaryTokens is the budget its own summary shares with a peer summary
	// (agent.ContextConfig.ThreadSummaryTokens in force), 0 when unknown or
	// off. Peer summaries written for it are sized from it.
	SummaryTokens int
	// CompactAt is the estimated input at which the participant compacts
	// its context (agent.ThreadView.CompactAt), 0 when unknown.
	CompactAt int
	Busy      bool // an activation is running
}

// Activation starts one reply of a participant.
type Activation struct {
	Epoch      uint64
	Foreground bool
	// Trigger is the entry that starts it: user_input, owner_change (a
	// transfer, possibly with a brief), assign, or report (a wake).
	Trigger Entry
	// Input is the text the participant replies to. Its reply input carries
	// Trigger.Seq so the cursor moves with it.
	Input string
	// Sync is applied before the reply starts; nil applies nothing.
	Sync *Sync
	// Depth is the assignment depth: 0 for user work, n+1 for a task assigned
	// by an activation at depth n.
	Depth int
}

// ActivationEvent is one item of an activation's stream. Exactly one item,
// the last, has End set; every other item has Event set.
type ActivationEvent struct {
	Path  string // "" for the participant's main agent, the member name for a sub-agent
	Event event.Event
	End   *ActivationEnd
}

// ActivationEnd closes an activation.
type ActivationEnd struct {
	Reason types.ReplyFinishedReason
	Err    error
	// Text is what the main agent showed: the text of its model rounds since
	// the activation started (or since the last Transfer sealed it), joined.
	Text      string
	Digest    *ToolDigest
	ToolCalls int // tool calls that started executing
}

// Participant wraps one agent (or a master agent with sub-agents) for the
// Coordinator. Implementations: package thread/participant.
type Participant interface {
	Descriptor() Descriptor
	// Bind gives the participant its Host. NewCoordinator calls it once. A
	// participant that also implements Releaser can be bound again after its
	// Coordinator releases it.
	Bind(h Host) error
	State() ParticipantState
	// Sync applies an update between activations.
	Sync(ctx context.Context, s Sync) error
	// Compact strictly compacts the participant's own context between
	// activations; on failure the state is unchanged.
	Compact(ctx context.Context, prompt string) (bool, error)
	// Activate starts one activation. The channel closes after the End item.
	// When act.Sync is not applied, or its outcome is unknown, Activate
	// returns an error and runs nothing; a channel means the Sync was
	// applied, even when its End is an error. The Coordinator records a
	// clear's reset only then.
	Activate(ctx context.Context, act Activation) (<-chan ActivationEvent, error)
	// SubmitUserConfirm answers a confirmation request the activation emitted.
	SubmitUserConfirm(path string, result *event.UserConfirmResultEvent) error
}

// Releaser is implemented by participants that accept one Host at a time.
// Close calls Release with the Host the Coordinator bound, after every
// activation and fork job has ended, and a NewCoordinator that fails after
// binding releases what it bound. A new Coordinator, for example one over the
// reopened log after a restart, can then Bind the same participant.
type Releaser interface {
	Release(h Host)
}

// Snapshot is a participant's frozen last request plus everything since.
type Snapshot interface {
	// Cursor is the participant's thread cursor when the snapshot was taken.
	Cursor() uint64
}

// ForkSpec asks a forked request for a summary.
type ForkSpec struct {
	Prompt      string
	Appendix    string
	MaxTokens   int
	Validate    func(text string) (feedback string, ok bool)
	MaxRewrites int
}

// ForkOutput is a fork's result.
type ForkOutput struct {
	Text     string
	Usage    *model.ChatUsage
	Calls    int
	Rewrites int
	Rejected bool
}

// ErrNoSnapshot means the participant has no safe snapshot right now.
var ErrNoSnapshot = errors.New("thread: no snapshot available")

// ErrStale means a snapshot no longer matches the participant's state.
var ErrStale = errors.New("thread: snapshot is stale")

// Forker is implemented by participants that write summaries by forking their
// last request (cloud participants), reusing the provider's prompt cache.
type Forker interface {
	Snapshot() (Snapshot, error)
	Fork(ctx context.Context, snap Snapshot, spec ForkSpec) (ForkOutput, error)
	// OfferOwnSummary hands the participant text, a summary of its own
	// context built from snap, to install when its context next reaches its
	// compaction line, without a model call (agent.OfferSummary). It may be
	// called while an activation runs; ErrStale when snap is outdated.
	OfferOwnSummary(ctx context.Context, snap Snapshot, text string) error
}

// ---------------------------------------------------------------- host

// TransferRequest hands the foreground to another participant.
type TransferRequest struct {
	From  Origin
	Epoch uint64 // the epoch of the calling activation
	To    string
	Brief Brief
	// Text and Digest are what the caller showed since the activation started;
	// they are sealed as its reply in the same append as the owner change.
	Text   string
	Digest *ToolDigest
	// EventsBefore is the number of ActivationEvent items (with Event set)
	// the participant had sent on the activation's channel before this call.
	// The Coordinator still forwards those to the user after the foreground
	// moves, so everything sealed as shown was actually shown: in order,
	// before the entry events of the transfer and before any event of the
	// new owner, whose activation waits until they are forwarded or the
	// channel closes. Zero means the participant does not report it: nothing
	// the Coordinator reads from the channel after the commit is forwarded.
	EventsBefore int
}

// AssignRequest starts a background task on another participant.
type AssignRequest struct {
	From   Origin
	Epoch  uint64
	Depth  int // depth of the calling activation
	To     string
	Text   string
	Notify bool
	Refs   []ArtifactRef
}

// ReportRequest publishes a result to other participants.
type ReportRequest struct {
	From   Origin
	Text   string
	To     []string
	TaskID string
	Notify bool
	Refs   []ArtifactRef
}

// PublishRequest records an artifact.
type PublishRequest struct {
	From Origin
	Refs []ArtifactRef
	Text string
}

// ReadQuery selects entries for thread_read.
type ReadQuery struct {
	After uint64
	Seqs  []uint64 // when set, exactly these entries (After is ignored)
	Kinds []Kind
	Limit int
}

// ErrInterrupted is returned by a Host write (Transfer, Assign, Report,
// Publish) when the calling activation was canceled, by Coordinator.Interrupt
// for example, or the context passed to the write ended, before the write
// committed. Nothing was written. The error also wraps the cancellation
// cause, such as context.Canceled.
var ErrInterrupted = errors.New("thread: the calling activation was interrupted")

// Host is what a participant and its tools see of the Coordinator. Every
// method is safe for concurrent use.
//
// Writes check, in the same step as their append, that the Coordinator is
// open (ErrClosed otherwise), and that neither the caller's running
// activation nor ctx was canceled (ErrInterrupted otherwise). A write made
// while the caller has no running activation checks only ctx.
type Host interface {
	Transfer(ctx context.Context, req TransferRequest) (uint64, error)
	Assign(ctx context.Context, req AssignRequest) (taskID string, seq uint64, err error)
	Report(ctx context.Context, req ReportRequest) (uint64, error)
	Publish(ctx context.Context, req PublishRequest) (uint64, error)
	Read(ctx context.Context, reader string, q ReadQuery) ([]Entry, error)
	EventOutline(reader string, limit int) []EventSummary
	EventSearch(reader string, query string, limit int) []EventSummary
	EventPage(reader string, id string, offset, limit int) (string, int, error)
	RecordEvent(r EventRecord) string
	RecordUsage(e LedgerEntry)
	// Pending is the participant's boundary input: entries for it past
	// cursor that arrived while it runs, as a Sync (nil when none).
	Pending(ctx context.Context, participant string, cursor uint64) (*Sync, error)
	Descriptors() []Descriptor
	// Owner returns the current owner and epoch.
	Owner() (string, uint64)
}
