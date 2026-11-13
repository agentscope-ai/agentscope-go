package participant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// Thread tool names.
const (
	ToolTransfer        = "transfer"
	ToolAssignTask      = "assign_task"
	ToolReport          = "report"
	ToolPublishArtifact = "publish_artifact"
	ToolThreadRead      = "thread_read"
	ToolEventRead       = "event_read"
)

// allToolNames lists the thread tools in installation order.
var allToolNames = []string{ToolTransfer, ToolAssignTask, ToolReport, ToolPublishArtifact, ToolThreadRead, ToolEventRead}

// writeTools are the external thread tools the participant answers itself.
var writeTools = map[string]bool{ToolTransfer: true, ToolAssignTask: true, ToolReport: true, ToolPublishArtifact: true}

// threadReadMaxRunes caps one thread_read output.
const threadReadMaxRunes = 6000

// defaultEventOutline is the number of records event_read lists by default;
// maxEventOutline caps what one outline or search may ask for.
const (
	defaultEventOutline = 20
	maxEventOutline     = 40
)

func isThreadTool(name string) bool {
	return slices.Contains(allToolNames, name)
}

// threadTool is one thread tool bound to its participant.
type threadTool struct {
	tool.BaseTool
	c        *core
	external bool
}

func newThreadTool(c *core, name string) *threadTool {
	t := &threadTool{c: c, external: writeTools[name]}
	t.ToolName = name
	t.ToolDescription = toolDescriptions[name]
	t.ToolSchema = json.RawMessage(toolSchemas[name])
	t.ReadOnly = !t.external
	t.ConcurrencySafe = !t.external
	return t
}

// IsExternalTool reports true for the writing tools: the participant answers
// them from its event forwarder.
func (t *threadTool) IsExternalTool() bool { return t.external }

// CheckPermissions allows every thread tool; deny and ask rules still apply.
func (t *threadTool) CheckPermissions(map[string]any, *permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.BehaviorAllow, Message: "thread tool " + t.ToolName}
}

// Execute runs the reading tools. The writing tools only run through the
// participant during an activation.
func (t *threadTool) Execute(ctx context.Context, input map[string]any) (*tool.ToolResponse, error) {
	if t.external {
		return failf("%s is answered by the thread participant during an activation; it cannot run here", t.ToolName), nil
	}
	act := t.c.current()
	if act == nil {
		return failf("%s only works while this participant is running an activation", t.ToolName), nil
	}
	host := t.c.hostOf()
	if t.ToolName == ToolThreadRead {
		return t.c.threadRead(ctx, host, input), nil
	}
	return t.c.eventRead(host, input), nil
}

func failf(format string, args ...any) *tool.ToolResponse {
	return tool.NewErrorResponse(fmt.Errorf(format, args...))
}

var toolDescriptions = map[string]string{
	ToolTransfer: "Hand the conversation with the user to another participant. Use it when that participant is better placed " +
		"to continue: it has the tools or data the task needs, the task needs stronger reasoning, or it is simple enough " +
		"for the edge. The receiver does not share your context, so the brief must stand on its own: the goal, what is " +
		"done, what remains, the constraints and any artifact URIs. Call it by itself; once it succeeds, stop.",
	ToolAssignTask: "Give a background task to another participant. The task text must stand on its own because the " +
		"assignee does not share your context. Returns a task ID; the result arrives later as a report. Set notify to " +
		"see the result at your next step.",
	ToolReport: "Publish an intermediate result to other participants (to: their names; empty means anyone reading the " +
		"thread). Give task_id when it belongs to an assigned task and notify to wake the recipients. A background " +
		"task's final answer is reported automatically.",
	ToolPublishArtifact: "Record an artifact (a file, document or URL) in the shared thread so other participants can find it.",
	ToolThreadRead: "Read entries of the shared thread: those after a seq, or exact seqs, optionally only some kinds. " +
		"The output is capped; page with after = the last seq shown, and read the rest of a cut entry with seqs and offset.",
	ToolEventRead: "Read tool outputs other participants produced. action outline lists recent records, search finds " +
		"records whose tool, target or output contains query, page reads one record's output from offset.",
}

var toolSchemas = map[string]string{
	ToolTransfer: `{"type":"object","properties":{` +
		`"to":{"type":"string","description":"Name of the participant that takes over."},` +
		`"goal":{"type":"string","description":"What the user wants, in one or two sentences."},` +
		`"done":{"type":"array","items":{"type":"string"},"description":"What is already done."},` +
		`"remaining":{"type":"array","items":{"type":"string"},"description":"What is left to do; at least one item."},` +
		`"constraints":{"type":"array","items":{"type":"string"},"description":"Rules the receiver must follow."},` +
		`"refs":{"type":"array","items":{"type":"string"},"description":"URIs of artifacts the receiver needs."}},` +
		`"required":["to","goal","remaining"],"additionalProperties":false}`,
	ToolAssignTask: `{"type":"object","properties":{` +
		`"to":{"type":"string","description":"Name of the participant that does the task."},` +
		`"task":{"type":"string","description":"The task, complete on its own."},` +
		`"notify":{"type":"boolean","description":"Show you the result at your next step."}},` +
		`"required":["to","task"],"additionalProperties":false}`,
	ToolReport: `{"type":"object","properties":{` +
		`"text":{"type":"string","description":"The result to publish."},` +
		`"to":{"type":"array","items":{"type":"string"},"description":"Recipients; empty means anyone reading the thread."},` +
		`"task_id":{"type":"string","description":"The assigned task this belongs to, if any."},` +
		`"notify":{"type":"boolean","description":"Wake the recipients."}},` +
		`"required":["text"],"additionalProperties":false}`,
	ToolPublishArtifact: `{"type":"object","properties":{` +
		`"uri":{"type":"string","description":"Where the artifact is."},` +
		`"summary":{"type":"string","description":"What it holds, in one line."},` +
		`"size":{"type":"integer","description":"Size in bytes, if known."},` +
		`"text":{"type":"string","description":"An optional note."}},` +
		`"required":["uri"],"additionalProperties":false}`,
	ToolThreadRead: `{"type":"object","properties":{` +
		`"after":{"type":"integer","minimum":0,"description":"Read entries with a larger seq."},` +
		`"seqs":{"type":"array","items":{"type":"integer","minimum":0},"description":"Read exactly these entries; after is ignored."},` +
		`"kinds":{"type":"array","items":{"type":"string","enum":["user_input","reply","summary","owner_change","assign","report","artifact","marker"]},"description":"Only these kinds."},` +
		`"limit":{"type":"integer","minimum":0,"description":"At most this many entries."},` +
		`"offset":{"type":"integer","minimum":0,"description":"Skip this many characters of the first entry shown, to read the rest of an entry that was cut."}},` +
		`"additionalProperties":false}`,
	ToolEventRead: `{"type":"object","properties":{` +
		`"action":{"type":"string","enum":["outline","search","page"]},` +
		`"query":{"type":"string","description":"Text to search for (search)."},` +
		`"id":{"type":"string","description":"Record ID such as ev-3 (page)."},` +
		`"offset":{"type":"integer","minimum":0,"description":"First character to read (page)."},` +
		`"limit":{"type":"integer","minimum":0,"description":"Records to list, or characters to read."}},` +
		`"required":["action"],"additionalProperties":false}`,
}

// SystemPromptSection returns the text a host appends to a participant's
// system prompt: who the participant is and its peers (thread.RenderPeers).
// It is byte-stable for the same input, whatever the order of all, because it
// sits in a cached prompt prefix. It is empty when there are no peers.
func SystemPromptSection(self thread.Descriptor, all []thread.Descriptor) string { //nolint:gocritic // the value parameter keeps the documented API
	peers := thread.RenderPeers(self.Name, all)
	if peers == "" {
		return ""
	}
	return "You are participant " + self.Name + " (zone " + string(self.Zone) + ") in a session shared through one " +
		"thread log. Each participant has its own context and tools; only what it publishes reaches the others. " +
		"Hand the conversation over with transfer when another participant is better placed to continue.\n" + peers
}

// ---------------------------------------------------------------- arguments

func strArg(in map[string]any, key string) string {
	s, _ := in[key].(string)
	return strings.TrimSpace(s)
}

func boolArg(in map[string]any, key string) bool {
	b, _ := in[key].(bool)
	return b
}

func intArg(in map[string]any, key string) int {
	switch n := in[key].(type) {
	case float64:
		if n >= 0 && n <= math.MaxInt32 {
			return int(n)
		}
	case int:
		return max(n, 0)
	case json.Number:
		if v, err := n.Int64(); err == nil && v >= 0 && v <= math.MaxInt32 {
			return int(v)
		}
	}
	return 0
}

func strList(in map[string]any, key string) []string {
	raw, _ := in[key].([]any)
	var out []string
	for _, v := range raw {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func seqList(in map[string]any, key string) []uint64 {
	raw, _ := in[key].([]any)
	var out []uint64
	for _, v := range raw {
		if seq, ok := seqArg(v); ok {
			out = append(out, seq)
		}
	}
	return out
}

// seqArg reads a thread seq from a decoded JSON number.
func seqArg(v any) (uint64, bool) {
	switch n := v.(type) {
	case float64:
		if n >= 0 && n == math.Trunc(n) && n < math.Ldexp(1, 64) {
			return uint64(n), true
		}
	case int:
		if n >= 0 {
			return uint64(n), true
		}
	case json.Number:
		if v, err := n.Int64(); err == nil && v >= 0 {
			return uint64(v), true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------- writing tools

// runWrite answers one writing tool call and records the answer, so that a
// note can tell the agent about it when it does not record the answer itself
// (see tracker.notes). It runs on the forwarder, so the text, digest and
// flags it uses cover exactly the events before the call.
//
// A call is answered under the activation that asks for it. A restored agent
// can ask again for a call that was submitted but never answered (reply
// recovery re-drives it in a later reply); that call is then answered under
// the later activation, whose epoch, foreground, ownership and interruption
// checks apply.
func (t *tracker) runWrite(call *message.ToolCallBlock) message.ToolResultBlock {
	w := &writeRecord{name: call.Name}
	w.result = t.write(call, w)
	if t.writes == nil {
		t.writes = map[string]*writeRecord{}
	}
	if _, seen := t.writes[call.ID]; !seen {
		t.writeOrder = append(t.writeOrder, call.ID)
	}
	t.writes[call.ID] = w
	return w.result
}

// write runs one writing call and returns its answer. A write that commits
// sets w.seq, w.kind and w.detail.
func (t *tracker) write(call *message.ToolCallBlock, w *writeRecord) message.ToolResultBlock {
	in, err := call.ParseInput()
	if err != nil {
		return errorResult(call, "the arguments are not valid JSON: "+err.Error())
	}
	switch call.Name {
	case ToolTransfer:
		return t.transfer(call, in, w)
	case ToolAssignTask:
		return t.assign(call, in, w)
	case ToolReport:
		return t.report(call, in, w)
	default:
		return t.publish(call, in, w)
	}
}

// committed records a write the host committed as entry seq.
func (w *writeRecord) committed(seq uint64, kind thread.Kind, detail string) {
	w.seq, w.kind, w.detail = seq, kind, detail
}

// writeFailure explains a write the host did not commit. An interrupted or
// closed thread wrote nothing, and the model is told so in plain words.
func writeFailure(name string, err error) string {
	switch {
	case errors.Is(err, thread.ErrInterrupted):
		return name + " was not done: the reply was interrupted before the call was recorded, so nothing was written. " +
			"Call " + name + " again if it is still needed."
	case errors.Is(err, thread.ErrClosed):
		return name + " was not done: the thread is closed, so nothing was written."
	}
	return name + " failed: " + err.Error()
}

func (t *tracker) origin() thread.Origin {
	return thread.Origin{Participant: t.c.desc.Name}
}

func (t *tracker) transfer(call *message.ToolCallBlock, in map[string]any, w *writeRecord) message.ToolResultBlock {
	if !t.act.foreground {
		return errorResult(call, "transfer only works in a foreground activation. This is a background task: "+
			"use report for intermediate results; your final answer is reported automatically.")
	}
	to := strArg(in, "to")
	if to == "" {
		return errorResult(call, "transfer needs to: the name of the participant that takes over")
	}
	if to == t.c.desc.Name {
		return errorResult(call, "you already hold the foreground; transfer to another participant or keep answering")
	}
	target, known := t.descriptor(to)
	if !known {
		return errorResult(call, "unknown participant "+to+"; participants: "+strings.Join(t.peerNames(), ", "))
	}
	brief := thread.Brief{
		Goal:        strArg(in, "goal"),
		Done:        strList(in, "done"),
		Remaining:   strList(in, "remaining"),
		Constraints: strList(in, "constraints"),
	}
	for _, uri := range strList(in, "refs") {
		brief.Refs = append(brief.Refs, thread.ArtifactRef{URI: uri})
	}
	if err := brief.Validate(target.BriefMaxRunes); err != nil {
		return errorResult(call, "transfer refused: "+strings.TrimPrefix(err.Error(), "thread: ")+". Fix the brief and call transfer again.")
	}
	text, digest := t.text.String(), t.digest(call.ID)
	// Every event sent so far showed the text sealed here; the Coordinator
	// forwards that many before the transfer's entries and the new owner.
	seq, err := t.host.Transfer(t.act.ctx, thread.TransferRequest{
		From: t.origin(), Epoch: t.act.epoch, To: to, Brief: brief,
		Text: text, Digest: digest,
		EventsBefore: t.eventsSent(),
	})
	if errors.Is(err, thread.ErrNotOwner) {
		return errorResult(call, "the foreground already moved to another participant, so you can no longer transfer it. "+
			"Do not call transfer again; finish briefly.")
	}
	if err != nil {
		return errorResult(call, writeFailure(ToolTransfer, err))
	}
	w.committed(seq, thread.KindOwnerChange, ": the foreground went to "+to)
	t.seal()
	if t.markHandedOff != nil {
		t.markHandedOff()
	}
	return successResult(call, fmt.Sprintf("Transferred to %s (entry #%d). Stop here; %s continues with the user.", to, seq, to))
}

func (t *tracker) descriptor(name string) (thread.Descriptor, bool) {
	all := t.host.Descriptors()
	for i := range all {
		if all[i].Name == name {
			return all[i], true
		}
	}
	return thread.Descriptor{}, false
}

func (t *tracker) peerNames() []string {
	all := t.host.Descriptors()
	var names []string
	for i := range all {
		if all[i].Name != t.c.desc.Name {
			names = append(names, all[i].Name)
		}
	}
	slices.Sort(names)
	return names
}

func (t *tracker) assign(call *message.ToolCallBlock, in map[string]any, w *writeRecord) message.ToolResultBlock {
	to, task := strArg(in, "to"), strArg(in, "task")
	if to == "" || task == "" {
		return errorResult(call, "assign_task needs to (a participant name) and task (the work, complete on its own)")
	}
	if _, known := t.descriptor(to); !known {
		return errorResult(call, "unknown participant "+to+"; participants: "+strings.Join(t.peerNames(), ", "))
	}
	notify := boolArg(in, "notify")
	id, seq, err := t.host.Assign(t.act.ctx, thread.AssignRequest{
		From: t.origin(), Epoch: t.act.epoch, Depth: t.act.depth, To: to, Text: task, Notify: notify,
	})
	if err != nil {
		return errorResult(call, writeFailure(ToolAssignTask, err))
	}
	w.committed(seq, thread.KindAssign, fmt.Sprintf(": task %s went to %s", id, to))
	msg := fmt.Sprintf("Assigned task %s to %s (entry #%d). The result arrives later as a report", id, to, seq)
	if notify {
		msg += " and is shown to you at your next step."
	} else {
		msg += "; read it with thread_read when you need it."
	}
	return successResult(call, msg)
}

func (t *tracker) report(call *message.ToolCallBlock, in map[string]any, w *writeRecord) message.ToolResultBlock {
	text := strArg(in, "text")
	if text == "" {
		return errorResult(call, "report needs text: the result to publish")
	}
	seq, err := t.host.Report(t.act.ctx, thread.ReportRequest{
		From: t.origin(), Text: text, To: strList(in, "to"), TaskID: strArg(in, "task_id"),
		Notify: boolArg(in, "notify"),
	})
	if err != nil {
		return errorResult(call, writeFailure(ToolReport, err))
	}
	w.committed(seq, thread.KindReport, "")
	return successResult(call, fmt.Sprintf("Reported (entry #%d).", seq))
}

func (t *tracker) publish(call *message.ToolCallBlock, in map[string]any, w *writeRecord) message.ToolResultBlock {
	uri := strArg(in, "uri")
	if uri == "" {
		return errorResult(call, "publish_artifact needs uri: where the artifact is")
	}
	ref := thread.ArtifactRef{URI: uri, Summary: strArg(in, "summary"), Size: int64(intArg(in, "size"))}
	seq, err := t.host.Publish(t.act.ctx, thread.PublishRequest{
		From: t.origin(), Refs: []thread.ArtifactRef{ref}, Text: strArg(in, "text"),
	})
	if err != nil {
		return errorResult(call, writeFailure(ToolPublishArtifact, err))
	}
	w.committed(seq, thread.KindArtifact, "")
	return successResult(call, fmt.Sprintf("Published %s (entry #%d).", uri, seq))
}

// ---------------------------------------------------------------- reading tools

func (c *core) threadRead(ctx context.Context, host thread.Host, in map[string]any) *tool.ToolResponse {
	q := thread.ReadQuery{Seqs: seqList(in, "seqs"), Limit: intArg(in, "limit")}
	q.After, _ = seqArg(in["after"])
	for _, k := range strList(in, "kinds") {
		q.Kinds = append(q.Kinds, thread.Kind(k))
	}
	offset := intArg(in, "offset")
	entries, err := host.Read(ctx, c.desc.Name, q)
	if err != nil {
		return failf("thread_read failed: %v", err)
	}
	if len(entries) == 0 {
		return tool.NewTextResponse("No thread entries match.")
	}
	var sb strings.Builder
	used := 0
	var last uint64
	capped, first := false, true
	cutAt := 0 // where a single entry too long for the output was cut
	for i := range entries {
		e := &entries[i]
		text := c.renderForRead(entries[i : i+1])
		if text == "" {
			continue
		}
		start := 0
		if first {
			first = false
			if offset > 0 {
				r := []rune(text)
				if offset >= len(r) {
					return failf("thread_read offset %d is past the end of entry #%d, which has %d characters", offset, e.Seq, len(r))
				}
				text, start = string(r[offset:]), offset
				fmt.Fprintf(&sb, "[#%d from character %d]\n", e.Seq, offset)
			}
		}
		n := utf8.RuneCountInString(text)
		if used > 0 {
			n++ // the separating newline
		}
		if used+n > threadReadMaxRunes {
			if used == 0 {
				r := []rune(text)
				sb.WriteString(string(r[:threadReadMaxRunes]))
				last, cutAt = e.Seq, start+threadReadMaxRunes
			}
			capped = true
			break
		}
		if used > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(text)
		used += n
		last = e.Seq
	}
	switch {
	case cutAt > 0:
		fmt.Fprintf(&sb, "\n[Entry #%d cut at character %d. Read the rest with thread_read seqs=[%d] offset=%d; "+
			"later entries with thread_read after=%d.]", last, cutAt, last, cutAt, last)
	case capped:
		fmt.Fprintf(&sb, "\n[Output capped at %d characters. Continue with thread_read after=%d.]", threadReadMaxRunes, last)
	case q.Limit > 0 && len(entries) >= q.Limit:
		fmt.Fprintf(&sb, "\n[Limit reached; more entries may follow: thread_read after=%d.]", last)
	}
	return tool.NewTextResponse(sb.String())
}

// renderForRead renders one entry the way a projection does; summaries,
// which projections never render, get their own line.
func (c *core) renderForRead(one []thread.Entry) string {
	e := &one[0]
	if e.Kind == thread.KindSummary {
		target := ""
		if e.Summary != nil {
			target = " for " + e.Summary.Target
		}
		through := ""
		if e.Coverage != nil {
			through = fmt.Sprintf(", covering through #%d", e.Coverage.Through)
		}
		return fmt.Sprintf("[#%d summary from %s%s%s]\n%s", e.Seq, e.Origin, target, through, e.Text())
	}
	p := thread.Project(one, &thread.ProjectionSpec{Participant: c.desc.Name, IncludeOwn: true})
	parts := make([]string, 0, len(p.Messages))
	for _, m := range p.Messages {
		if t := m.GetTextContent("\n"); t != nil {
			parts = append(parts, *t)
		}
	}
	return strings.Join(parts, "\n")
}

func (c *core) eventRead(host thread.Host, in map[string]any) *tool.ToolResponse {
	name := c.desc.Name
	limit := intArg(in, "limit")
	switch strArg(in, "action") {
	case "outline":
		if limit == 0 {
			limit = defaultEventOutline
		}
		limit = min(limit, maxEventOutline)
		return renderOutline(host.EventOutline(name, limit), "No tool outputs are available to read.")
	case "search":
		query := strArg(in, "query")
		if query == "" {
			return failf("event_read search needs query")
		}
		if limit == 0 {
			limit = defaultEventOutline
		}
		limit = min(limit, maxEventOutline)
		return renderOutline(host.EventSearch(name, query, limit), "No tool outputs match "+query+".")
	case "page":
		id := strArg(in, "id")
		if id == "" {
			return failf("event_read page needs id, such as ev-3 from outline or search")
		}
		offset := intArg(in, "offset")
		text, total, err := host.EventPage(name, id, offset, limit)
		if err != nil {
			return failf("event_read page %s failed: %v", id, err)
		}
		end := min(offset+utf8.RuneCountInString(text), total)
		var sb strings.Builder
		fmt.Fprintf(&sb, "%s characters %d-%d of %d:\n", id, min(offset, total), end, total)
		sb.WriteString(text)
		if end < total {
			fmt.Fprintf(&sb, "\n[More: event_read action=page id=%s offset=%d]", id, end)
		}
		return tool.NewTextResponse(sb.String())
	default:
		return failf("event_read action must be outline, search or page")
	}
}

func renderOutline(list []thread.EventSummary, empty string) *tool.ToolResponse {
	if len(list) == 0 {
		return tool.NewTextResponse(empty)
	}
	var sb strings.Builder
	for i := range list {
		s := &list[i]
		if i > 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "%s %s", s.ID, s.Tool)
		if s.Target != "" {
			sb.WriteString(" " + s.Target)
		}
		fmt.Fprintf(&sb, " (from %s, %d characters)", s.Origin, s.Size)
		if s.FirstLine != "" {
			sb.WriteString(": " + s.FirstLine)
		}
	}
	return tool.NewTextResponse(sb.String())
}
