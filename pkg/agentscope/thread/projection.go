package thread

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// SeqMetadataKey is the Msg.Metadata key holding the entry seq. It equals
// agent.ThreadSeqMetadataKey.
const SeqMetadataKey = "thread_seq"

// KindMetadataKey is the Msg.Metadata key holding the kind of the entry a
// projected message renders.
const KindMetadataKey = "thread_kind"

// projectionSender is the name on every projected message.
const projectionSender = "thread"

// ProjectionSpec says what a participant imports from the log.
type ProjectionSpec struct {
	// Participant is the importing participant.
	Participant string
	// After is the participant's cursor; entries at or before it are ignored.
	After uint64
	// Covered is the coverage of the newest summary the participant holds.
	// Entries it covers are skipped.
	Covered *Coverage
	// MaxTokens is the budget for the rendered messages; 0 means unlimited.
	MaxTokens int
	// Count counts a message's tokens; nil counts (runes+3)/4 of its text.
	Count func(*message.Msg) int
	// IncludeOwn renders entries this participant wrote. Only crash recovery
	// needs it; the participant otherwise already holds them.
	IncludeOwn bool
	// OwnBasedAfter, when positive, also renders the participant's own
	// entries based on a seq after it, and its own progress entries that are
	// still live: after a restart, what an activation wrote past the
	// checkpoint may be missing from the context, and a checkpoint taken
	// mid-run has the run's input (so the cursor) but maybe not its newest
	// plan.
	OwnBasedAfter uint64
}

// Projection is the result of Project.
type Projection struct {
	// Messages are the rendered messages, oldest first.
	Messages []*message.Msg
	// Through is the last seq consumed (rendered or skipped): the
	// participant's new cursor. It equals After when nothing was consumed.
	Through uint64
	// Cut lists the entries rendered only in part because a first message
	// alone exceeded MaxTokens. The importer has consumed them without
	// seeing them in full, so a summary it writes must not claim them.
	Cut []uint64
	// Remaining is set when the projection stopped at MaxTokens with entries
	// left.
	Remaining bool
}

// Project turns log entries into the text messages a participant imports.
// Entries must be in seq order. It is a pure function.
//
// Summary entries are consumed but never rendered; they belong in a separate
// summary slot. Entries covered by spec.Covered, the participant's own
// entries unless IncludeOwn (or OwnBasedAfter lets them through), and
// progress entries a later entry replaces are skipped. When MaxTokens is set the projection stops before the first message that
// would exceed it; a first message that alone exceeds it is cut so the
// projection always makes progress. Project does not change spec; a nil
// spec is the zero one.
func Project(entries []Entry, spec *ProjectionSpec) Projection {
	if spec == nil {
		spec = &ProjectionSpec{}
	}
	p := projector{spec: *spec, count: spec.Count, out: Projection{Through: spec.After}}
	if p.count == nil {
		p.count = estimateTokens
	}
	stale := staleProgress(entries)
	for i := range entries {
		e := &entries[i]
		if e.Seq <= spec.After {
			continue
		}
		skip := e.Kind == KindSummary || stale[e.Seq] ||
			spec.Covered.Covers(e.Seq) ||
			(!spec.IncludeOwn && spec.Participant != "" && e.Origin.Participant == spec.Participant &&
				(spec.OwnBasedAfter == 0 || (e.BasedOn <= spec.OwnBasedAfter && e.Kind != KindProgress)))
		if skip {
			p.out.Through = e.Seq
			continue
		}
		if !p.add(projectedMsg(renderEntry(e), e.Seq, string(e.Kind)), e.Seq) {
			return p.out
		}
		p.out.Through = e.Seq
	}
	return p.out
}

// staleProgress returns the progress entries that a later entry in entries
// replaces: a newer progress entry of the same run, or the run's end (its
// reply, transfer, late report, task report or interrupted marker) unless
// the run failed. The
// newest progress of a failed run stays: it is what a failover or a restart
// needs. Entries past the slice cannot replace anything, so a projection of
// a prefix keeps the newest progress it holds.
func staleProgress(entries []Entry) map[uint64]bool {
	var stale map[uint64]bool
	newest := make(map[string]uint64) // run -> its newest progress seq
	ended := make(map[string]bool)
	failed := make(map[string]bool)
	for i := range entries {
		e := &entries[i]
		switch e.Kind {
		case KindProgress:
			if prev, ok := newest[e.Run]; ok {
				if stale == nil {
					stale = make(map[uint64]bool)
				}
				stale[prev] = true
			}
			newest[e.Run] = e.Seq
		case KindMarker:
			if run, ok := strings.CutPrefix(e.DedupKey, failedPrefix); ok {
				failed[run] = true
			} else if run, ok := strings.CutPrefix(e.DedupKey, interruptedPrefix); ok {
				ended[run] = true
			}
		default:
			for _, p := range runEndPrefixes {
				if run, ok := strings.CutPrefix(e.DedupKey, p); ok {
					ended[run] = true
				}
			}
		}
	}
	for run, seq := range newest {
		if ended[run] && !failed[run] {
			if stale == nil {
				stale = make(map[uint64]bool)
			}
			stale[seq] = true
		}
	}
	return stale
}

// failedPrefix starts the dedup key of a run's failed marker; the rest is
// the run's activation ID.
const failedPrefix = "failed:"

// interruptedPrefix starts the dedup key of the interrupted marker of an
// activation: "interrupted:" + its ID.
const interruptedPrefix = "interrupted:"

// runEndPrefixes start the dedup keys of the entries that end a run; the
// rest is the run's activation ID.
var runEndPrefixes = []string{"reply:", "transfer:", "late:", taskReportPrefix}

type projector struct {
	spec  ProjectionSpec
	count func(*message.Msg) int
	used  int
	out   Projection
}

// add appends msg when it fits the budget, or when it is the first message,
// cutting it first. It reports false, and sets Remaining, when the budget
// stopped the projection.
func (p *projector) add(msg *message.Msg, cutSeq uint64) bool {
	n := p.count(msg)
	if limit := p.spec.MaxTokens; limit > 0 && p.used+n > limit {
		if len(p.out.Messages) > 0 {
			p.out.Remaining = true
			return false
		}
		n = p.cut(msg, cutSeq)
		p.out.Cut = append(p.out.Cut, cutSeq)
	}
	p.out.Messages = append(p.out.Messages, msg)
	p.used += n
	return true
}

// cut shortens msg's text to the longest prefix that, with the cut notice
// appended, fits MaxTokens, and returns its new count. When even the notice
// alone does not fit, msg keeps only the notice.
func (p *projector) cut(msg *message.Msg, seq uint64) int {
	runes := []rune(msgText(msg))
	notice := fmt.Sprintf("… [cut; read #%d in full with thread_read]", seq)
	try := func(k int) int {
		setMsgText(msg, string(runes[:k])+notice)
		return p.count(msg)
	}
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if try(mid) <= p.spec.MaxTokens {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return try(lo)
}

func projectedMsg(text string, seq uint64, kind string) *message.Msg {
	m := message.UserMsg(projectionSender, text)
	m.Metadata[SeqMetadataKey] = seq
	m.Metadata[KindMetadataKey] = kind
	return m
}

func msgText(m *message.Msg) string {
	if t := m.GetTextContent("\n"); t != nil {
		return *t
	}
	return ""
}

func setMsgText(m *message.Msg, text string) {
	id := ""
	if len(m.Content) > 0 {
		id = m.Content[0].GetID()
	}
	m.Content = []message.ContentBlock{message.TextBlock{Type: string(message.ContentBlockText), ID: id, Text: text}}
}

// estimateTokens is the default counter: (runes+3)/4 of the text.
func estimateTokens(m *message.Msg) int {
	return (utf8.RuneCountInString(msgText(m)) + 3) / 4
}

// renderEntry formats one entry: a header bracket, inline text, then any
// further lines.
func renderEntry(e *Entry) string {
	var head, inline string
	var extra []string
	text := e.Text()
	switch e.Kind {
	case KindUserInput:
		head = fmt.Sprintf("[#%d user]", e.Seq)
		inline = text
	case KindReply:
		head = fmt.Sprintf("[#%d reply from %s]", e.Seq, e.Origin)
		inline = text
		if d := e.Digest.Render(); d != "" {
			extra = append(extra, "Tools:\n"+d)
		}
	case KindOwnerChange:
		var o OwnerChange
		if e.Owner != nil {
			o = *e.Owner
		}
		head = fmt.Sprintf("[#%d foreground: %s → %s", e.Seq, nameOrNone(o.From), nameOrNone(o.To))
		if o.Reason != "" {
			head += ", reason " + string(o.Reason)
		}
		head += "]"
		inline = text
		if o.Brief != nil {
			extra = append(extra, "Brief:\n"+o.Brief.Render())
		}
	case KindAssign:
		id := ""
		if e.Task != nil {
			id = e.Task.ID
		}
		head = fmt.Sprintf("[#%d task %s from %s%s]", e.Seq, id, e.Origin, toSuffix(e.To))
		inline = text
	case KindReport:
		head = fmt.Sprintf("[#%d report from %s%s]", e.Seq, e.Origin, toSuffix(e.To))
		inline = text
	case KindArtifact:
		head = fmt.Sprintf("[#%d artifact from %s]", e.Seq, e.Origin)
		for _, r := range e.Refs {
			extra = append(extra, renderRef(r))
		}
		if text != "" {
			extra = append(extra, text)
		}
		if len(extra) > 0 {
			inline, extra = extra[0], extra[1:]
		}
	case KindProgress:
		head = fmt.Sprintf("[#%d progress of %s]", e.Seq, e.Origin)
		if text != "" {
			extra = append(extra, text)
		}
	case KindMarker:
		kind := ""
		if e.Marker != nil {
			kind = string(e.Marker.Kind)
			inline = e.Marker.Detail
		}
		head = fmt.Sprintf("[#%d marker %s]", e.Seq, kind)
		if text != "" {
			extra = append(extra, text)
		}
	default:
		head = fmt.Sprintf("[#%d %s from %s]", e.Seq, e.Kind, e.Origin)
		inline = text
	}
	var sb strings.Builder
	sb.WriteString(head)
	if inline != "" {
		sb.WriteString(" ")
		sb.WriteString(inline)
	}
	for _, x := range extra {
		sb.WriteString("\n")
		sb.WriteString(x)
	}
	return sb.String()
}

func renderRef(r ArtifactRef) string {
	s := r.URI
	if r.Summary != "" {
		s += " — " + r.Summary
	}
	if r.Size > 0 {
		s += fmt.Sprintf(" (%d bytes)", r.Size)
	}
	return s
}

func nameOrNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func toSuffix(to []string) string {
	if len(to) == 0 {
		return ""
	}
	return " to " + strings.Join(to, ", ")
}

// firstLine returns the first non-blank line of s, trimmed.
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// truncateRunes cuts s to at most n runes, ending a cut string with "…".
func truncateRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
