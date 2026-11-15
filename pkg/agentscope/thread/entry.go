package thread

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// Kind names what an entry records.
type Kind string

const (
	// KindUserInput is a user message. It wakes the owner.
	KindUserInput Kind = "user_input"
	// KindReply is the text the user actually saw from a participant, with a
	// digest of the tools it used. It wakes nobody.
	KindReply Kind = "reply"
	// KindSummary is a summary a cloud fork wrote for a target window, with
	// the range of entries it covers. It wakes nobody.
	KindSummary Kind = "summary"
	// KindOwnerChange moves the foreground to a new owner and starts a new
	// epoch. It wakes the new owner.
	KindOwnerChange Kind = "owner_change"
	// KindAssign hands a background task to a participant. It wakes it.
	KindAssign Kind = "assign"
	// KindReport carries progress or a result. It wakes its recipients only
	// when Notify is set.
	KindReport Kind = "report"
	// KindArtifact records an artifact reference. It wakes nobody.
	KindArtifact Kind = "artifact"
	// KindMarker records an interruption, a recovery, unknown effects or the
	// absence of an owner.
	KindMarker Kind = "marker"
	// KindProgress is a run's plan as its agent's newest plan_updated event
	// left it: what the run had done and meant to do next. It wakes nobody.
	// A projection renders only the newest one of a run that has not ended,
	// or that ended in a failure, so a failover or a restart sees where the
	// run stood without contexts collecting stale plans.
	KindProgress Kind = "progress"
)

// Zone is where a participant runs. Every participant reads every entry.
type Zone string

const (
	// ZoneEdge runs on the device, with a small window.
	ZoneEdge Zone = "edge"
	// ZoneCloud runs off the device.
	ZoneCloud Zone = "cloud"
)

// Origin identifies who produced an entry: a participant and the agent path
// inside it, such as cloud/master. The user and the Coordinator have an
// empty participant.
type Origin struct {
	Participant string `json:"participant,omitempty"`
	Path        string `json:"path,omitempty"`
}

// String renders the origin as participant/path.
func (o Origin) String() string {
	switch {
	case o.Participant == "" && o.Path == "":
		return "coordinator"
	case o.Path == "":
		return o.Participant
	default:
		return o.Participant + "/" + o.Path
	}
}

// OwnerReason says why the foreground moved.
type OwnerReason string

const (
	// ReasonRouter is a routing decision at a query boundary. No model took
	// part and there is no brief.
	ReasonRouter OwnerReason = "router"
	// ReasonTransfer is a Transfer a participant's model asked for.
	ReasonTransfer OwnerReason = "transfer"
	// ReasonFailover moves the foreground because the owner is unreachable or
	// its activation failed.
	ReasonFailover OwnerReason = "failover"
	// ReasonUser is an explicit user choice.
	ReasonUser OwnerReason = "user"
)

// OwnerChange is the payload of an owner_change entry.
type OwnerChange struct {
	From   string      `json:"from,omitempty"`
	To     string      `json:"to"`
	Reason OwnerReason `json:"reason"`
	Brief  *Brief      `json:"brief,omitempty"`
}

// Brief is the structured handoff a Transfer carries.
type Brief struct {
	Goal        string        `json:"goal"`
	Done        []string      `json:"done,omitempty"`
	Remaining   []string      `json:"remaining,omitempty"`
	Constraints []string      `json:"constraints,omitempty"`
	Refs        []ArtifactRef `json:"refs,omitempty"`
}

// Validate checks the required fields and the size limit. A non-positive
// maxRunes disables the size check.
func (b *Brief) Validate(maxRunes int) error {
	if b == nil {
		return errors.New("thread: brief is required")
	}
	if strings.TrimSpace(b.Goal) == "" {
		return errors.New("thread: brief needs a goal")
	}
	if len(b.Remaining) == 0 {
		return errors.New("thread: brief needs the remaining work")
	}
	if maxRunes > 0 {
		if n := len([]rune(b.Render())); n > maxRunes {
			return fmt.Errorf("thread: brief has %d characters, the receiver accepts at most %d", n, maxRunes)
		}
	}
	return nil
}

// Render formats the brief as the text a receiver reads.
func (b *Brief) Render() string {
	if b == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Goal: ")
	sb.WriteString(strings.TrimSpace(b.Goal))
	list := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		sb.WriteString("\n")
		sb.WriteString(title)
		sb.WriteString(":")
		for _, it := range items {
			sb.WriteString("\n- ")
			sb.WriteString(strings.TrimSpace(it))
		}
	}
	list("Done", b.Done)
	list("Remaining", b.Remaining)
	list("Constraints", b.Constraints)
	if len(b.Refs) > 0 {
		sb.WriteString("\nArtifacts:")
		for _, r := range b.Refs {
			sb.WriteString("\n- ")
			sb.WriteString(r.URI)
			if r.Summary != "" {
				sb.WriteString(": ")
				sb.WriteString(r.Summary)
			}
		}
	}
	return sb.String()
}

// ToolDigest lists the tools a reply used, without their outputs.
type ToolDigest struct {
	Calls []ToolCallDigest `json:"calls"`
}

// ToolCallDigest is one tool call: its name and its raw arguments, cut at
// DigestArgsRunes.
type ToolCallDigest struct {
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

// Render formats the digest one call per line, as "read_file {"path":"a.go"}".
func (d *ToolDigest) Render() string {
	if d == nil || len(d.Calls) == 0 {
		return ""
	}
	parts := make([]string, 0, len(d.Calls))
	for _, c := range d.Calls {
		s := c.Name
		if c.Args != "" {
			s += " " + c.Args
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
}

// ArtifactRef points at an artifact outside the log.
type ArtifactRef struct {
	URI     string `json:"uri"`
	Summary string `json:"summary,omitempty"`
	Size    int64  `json:"size,omitempty"`
}

// Coverage is the range a summary covers, recorded by the Coordinator before
// the fork, never inferred from messages or the model. Every entry with
// Seq <= Through is in the snapshot except the ones listed in Excluded:
// entries of other participants past the snapshot's cursor.
type Coverage struct {
	Through  uint64   `json:"through"`
	Excluded []uint64 `json:"excluded,omitempty"`
}

// Covers reports whether the coverage includes seq.
func (c *Coverage) Covers(seq uint64) bool {
	if c == nil || seq > c.Through {
		return false
	}
	return !slices.Contains(c.Excluded, seq)
}

// SummaryMeta is the payload of a summary entry.
type SummaryMeta struct {
	// Target is the participant whose window the summary was written for.
	Target string `json:"target"`
	// Budget is the token budget the fork was asked to meet.
	Budget int `json:"budget,omitempty"`
	// Lossy is set when the summary was cut to fit after its rewrites ran out.
	Lossy bool `json:"lossy,omitempty"`
}

// MarkerKind names a marker.
type MarkerKind string

const (
	// MarkerInterrupted records a canceled activation.
	MarkerInterrupted MarkerKind = "interrupted"
	// MarkerRecovered records a request a restart left unanswered and did
	// not resume: it waits for the user's next message.
	MarkerRecovered MarkerKind = "recovered"
	// MarkerRestarted records that the process stopped while a request was
	// being answered and the restarted Coordinator resumed it; the detail
	// carries the host's account (Config.RecoveryNote).
	MarkerRestarted MarkerKind = "restarted"
	// MarkerFailed records an activation that ended with an error, and why.
	MarkerFailed MarkerKind = "failed"
	// MarkerUnknownEffects records that tools may have run with no recorded
	// result, so the next owner must check state before repeating them.
	MarkerUnknownEffects MarkerKind = "unknown_effects"
	// MarkerNoOwner records that no participant can take the foreground.
	MarkerNoOwner MarkerKind = "no_owner"
	// MarkerRejected records late output from a participant that was no
	// longer the owner. The text is kept as a report.
	MarkerRejected MarkerKind = "rejected_output"
)

// Marker is the payload of a marker entry.
type Marker struct {
	Kind   MarkerKind `json:"kind"`
	Detail string     `json:"detail,omitempty"`
}

// Task is the payload of an assign entry and of the reports about its task.
type Task struct {
	ID string `json:"id"`
	// Notify asks the assignee to wake the assigner with its final report.
	Notify bool `json:"notify,omitempty"`
	// End is set only on the report that ends the task, and says how it
	// ended: the reason the assignee's run finished with, or TaskUnfinished.
	// Progress reports leave it empty. It records what happened; what a
	// host makes of it, such as a ledger entry, is the host's choice.
	End types.ReplyFinishedReason `json:"end,omitempty"`
}

// TaskUnfinished is the End of the report recovery writes for a task that
// had not ended when the session stopped: no result was recorded, so its
// effects are unknown.
const TaskUnfinished types.ReplyFinishedReason = "unfinished"

// Entry is one record in the Main Thread. Content never holds tool calls,
// tool results or thinking.
type Entry struct {
	Seq      uint64                 `json:"seq"`
	Epoch    uint64                 `json:"epoch"`
	Kind     Kind                   `json:"kind"`
	Origin   Origin                 `json:"origin"`
	To       []string               `json:"to,omitempty"`
	BasedOn  uint64                 `json:"based_on,omitempty"`
	Content  []message.ContentBlock `json:"-"`
	Digest   *ToolDigest            `json:"digest,omitempty"`
	Refs     []ArtifactRef          `json:"refs,omitempty"`
	Coverage *Coverage              `json:"coverage,omitempty"`
	Summary  *SummaryMeta           `json:"summary,omitempty"`
	Owner    *OwnerChange           `json:"owner,omitempty"`
	Task     *Task                  `json:"task,omitempty"`
	Marker   *Marker                `json:"marker,omitempty"`
	Notify   bool                   `json:"notify,omitempty"`
	// Run is set on a progress entry: the ID of the activation it reports on.
	Run string `json:"run,omitempty"`
	// Steered lists, on the entry that ends a run, the user inputs the run
	// took besides its trigger: older inputs its cycle carried as context,
	// inputs delivered to it while it ran, inputs a recovery joined to the
	// request it resumed, and a failed run's inputs its failover took over.
	// The entry is the run's failed or interrupted marker, else its reply,
	// else the owner change of its Transfer. After a reply or a Transfer
	// recovery takes none of them for an open request; after a failure or
	// an interrupt they share the fate of the run's request. A failed wake
	// queues them again; an interrupted one ends them.
	Steered  []uint64  `json:"steered,omitempty"`
	DedupKey string    `json:"dedup_key,omitempty"`
	Time     time.Time `json:"time"`
}

// Text joins the entry's text blocks.
func (e *Entry) Text() string {
	var parts []string
	for _, b := range e.Content {
		if t, ok := b.(message.TextBlock); ok && t.Text != "" {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Addressed reports whether the entry names participant p as a recipient.
func (e *Entry) Addressed(p string) bool {
	return slices.Contains(e.To, p)
}

// entryAlias is Entry without its JSON methods.
type entryAlias Entry

type entryJSON struct {
	entryAlias
	Content json.RawMessage `json:"content,omitempty"`
}

// MarshalJSON encodes the entry with its content blocks.
func (e Entry) MarshalJSON() ([]byte, error) { //nolint:gocritic // a value receiver keeps the content when an Entry value is marshaled
	var raw json.RawMessage
	if len(e.Content) > 0 {
		b, err := json.Marshal(e.Content)
		if err != nil {
			return nil, fmt.Errorf("thread: marshal entry content: %w", err)
		}
		raw = b
	}
	return json.Marshal(entryJSON{entryAlias: entryAlias(e), Content: raw})
}

// UnmarshalJSON decodes the entry and its content blocks.
func (e *Entry) UnmarshalJSON(data []byte) error {
	var raw entryJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("thread: unmarshal entry: %w", err)
	}
	*e = Entry(raw.entryAlias)
	e.Content = nil
	if len(raw.Content) > 0 && string(raw.Content) != "null" {
		blocks, err := message.UnmarshalContentBlocks(raw.Content)
		if err != nil {
			return fmt.Errorf("thread: unmarshal entry content: %w", err)
		}
		e.Content = blocks
	}
	return nil
}

// ValidateContent rejects tool calls, tool results and thinking, which stay
// in the agent that produced them.
func ValidateContent(blocks []message.ContentBlock) error {
	for _, b := range blocks {
		switch b.(type) {
		case message.ToolCallBlock, message.ToolResultBlock, message.ThinkingBlock, message.RedactedThinkingBlock,
			*message.ToolCallBlock, *message.ToolResultBlock, *message.ThinkingBlock, *message.RedactedThinkingBlock:
			return fmt.Errorf("thread: entry content may not hold %T", b)
		}
	}
	return nil
}

// Guard is the owner and epoch a user-visible write or an ownership change
// was made under.
type Guard struct {
	Owner string
	Epoch uint64
}

var (
	// ErrNotOwner is returned when a guarded write's owner or epoch no
	// longer matches the log.
	ErrNotOwner = errors.New("thread: writer is not the current owner")
	// ErrDuplicate is returned when a batch repeats some, but not all, of
	// the dedup keys of entries already in the log.
	ErrDuplicate = errors.New("thread: batch partly repeats entries already in the log")
	// ErrClosed is returned after a store or coordinator is closed.
	ErrClosed = errors.New("thread: closed")
)
