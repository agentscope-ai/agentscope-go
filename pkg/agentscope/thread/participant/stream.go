package participant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// partialResponseEvent is the CustomEvent the agent emits when a provider
// delivered a partial model response.
const partialResponseEvent = "model_partial_response"

// errNoTerminal reports a reply stream that closed without its ReplyEnd.
// planToolName is the tool agent.WithPlanTool installs.
const planToolName = "update_plan"

var errNoTerminal = errors.New("participant: the reply stream ended without a terminal event")

// callRecord is one tool call seen in the stream.
type callRecord struct {
	id, name, path string
	input          string // raw JSON arguments
	args           map[string]any
	state          message.ToolResultState
	done           bool
	rejected       bool // part of a step the agent refused to run
	output         strings.Builder
	cut            bool
}

// counts reports whether the call ran (or may have run): it was neither
// denied nor refused as part of a mixed exclusive step.
func (r *callRecord) counts() bool {
	return !r.rejected && r.state != message.ToolResultDenied
}

func (r *callRecord) displayName() string {
	if r.path == "" {
		return r.name
	}
	return r.path + "/" + r.name
}

// replyPath tracks one agent's reply inside the stream.
type replyPath struct {
	path      string
	modelOpen bool
	round     []*callRecord // tool calls since the agent's last model call
	calls     map[string]*callRecord
}

// tracker follows one activation's event stream on the forwarder goroutine:
// the main agent's visible text, the tool calls for the digest and the call
// count, the event list and the usage ledger. It also answers the participant's external thread tools at their
// exact place in the stream.
type tracker struct {
	c    *core
	act  *activation
	host thread.Host
	// team is set for a TeamParticipant: replies of agents other than
	// mainName are members.
	team     bool
	mainName string
	paths    map[string]*replyPath

	text       strings.Builder
	modelRound bool
	partial    bool
	sawModel   bool
	segment    []*callRecord // calls since the activation started or the last transfer
	all        []*callRecord
	mainEnd    *event.ReplyEndEvent
	// writes are the writing calls answered in this activation, by call ID;
	// writeOrder keeps the order they were answered in.
	writes     map[string]*writeRecord
	writeOrder []string

	// sent reports how many Event items the activation has delivered on its
	// channel so far; a Transfer passes it as EventsBefore. Nil reports 0.
	sent          func() int
	markHandedOff func()
	submit        func(req *event.RequireExternalExecutionEvent, results []message.ToolResultBlock)
	onConfirm     func(path string, req *event.RequireUserConfirmEvent)
}

// writeRecord is one writing call the participant answered, and whether the
// agent recorded that answer.
type writeRecord struct {
	name string
	// seq is the entry the write committed, 0 when it wrote nothing; detail
	// says what the committed entry did, for the note (see tracker.notes).
	seq    uint64
	kind   thread.Kind
	detail string
	result message.ToolResultBlock
	// output collects the result text the agent emitted for the call;
	// delivered is set when its end event matches result, which the agent
	// emits only once it recorded that result.
	output    strings.Builder
	delivered bool
}

func newTracker(c *core, act *activation, host thread.Host) *tracker {
	return &tracker{c: c, act: act, host: host, paths: map[string]*replyPath{}}
}

// handle processes one event and reports whether to forward it, and on which
// agent path.
func (t *tracker) handle(ev event.Event) (forward bool, path string) {
	if start, ok := ev.(event.ReplyStartEvent); ok {
		rp := &replyPath{calls: map[string]*callRecord{}}
		if t.team && start.Name != t.mainName {
			rp.path = start.Name
		}
		t.paths[start.ReplyID] = rp
		return true, rp.path
	}
	rp := t.paths[ev.GetReplyID()]
	if rp == nil {
		if t.team {
			return true, ""
		}
		rp = &replyPath{calls: map[string]*callRecord{}}
		t.paths[ev.GetReplyID()] = rp
	}
	main := rp.path == ""
	switch e := ev.(type) {
	case event.ModelCallStartEvent:
		t.finishRound(rp)
		rp.modelOpen = true
		if main {
			t.modelRound, t.partial, t.sawModel = true, false, true
		}
	case event.ModelCallEndEvent:
		t.recordUsage(rp, &e)
	case event.TextBlockStartEvent:
		if main && t.modelRound && t.text.Len() > 0 {
			t.text.WriteByte('\n')
		}
	case event.TextBlockDeltaEvent:
		if main && t.modelRound {
			t.text.WriteString(e.Delta)
		}
	case event.CustomEvent:
		if main && e.Name == partialResponseEvent {
			t.partial = true
		}
	case event.ToolCallStartEvent:
		t.startCall(rp, e.ToolCallID, e.ToolCallName, e.ToolCallInput)
	case event.ToolResultStartEvent:
		if r := rp.calls[e.ToolCallID]; r == nil || r.done {
			t.startCall(rp, e.ToolCallID, e.ToolCallName, "")
		}
	case event.ToolResultTextDeltaEvent:
		if r := rp.calls[e.ToolCallID]; r != nil {
			t.appendOutput(r, e.Delta)
		}
		if w := t.writeOf(rp, e.ToolCallID); w != nil {
			w.output.WriteString(e.Delta)
		}
	case event.ToolResultEndEvent:
		if r := rp.calls[e.ToolCallID]; r != nil {
			t.endCall(r, e.State)
		}
		if w := t.writeOf(rp, e.ToolCallID); w != nil {
			w.delivered = e.State == w.result.State && w.output.String() == w.result.GetOutputText()
		}
	case event.RequireUserConfirmEvent:
		if t.onConfirm != nil {
			t.onConfirm(rp.path, &e)
		}
	case event.RequireExternalExecutionEvent:
		t.answerExternal(rp, &e)
		return false, rp.path
	case event.ReplyEndEvent:
		t.finishRound(rp)
		if main {
			cp := e
			t.mainEnd = &cp
		}
	}
	return true, rp.path
}

// startCall opens a record for a call. A finished record with the same ID
// belongs to an earlier round: some providers (Gemini) number calls per
// response, so a later round of the same reply can repeat an ID, and that
// call gets a record of its own.
func (t *tracker) startCall(rp *replyPath, id, name, input string) {
	if r := rp.calls[id]; r != nil && !r.done {
		return
	}
	r := &callRecord{id: id, name: name, path: rp.path, input: input}
	if input != "" {
		var args map[string]any
		if json.Unmarshal([]byte(input), &args) == nil {
			r.args = args
		}
	}
	rp.calls[id] = r
	rp.round = append(rp.round, r)
	t.segment = append(t.segment, r)
	t.all = append(t.all, r)
}

func (t *tracker) appendOutput(r *callRecord, delta string) {
	if r.cut {
		return
	}
	room := t.c.maxOutput - r.output.Len()
	if len(delta) > room {
		delta = cutBytes(delta, room)
		r.cut = true
	}
	r.output.WriteString(delta)
}

func (t *tracker) endCall(r *callRecord, state message.ToolResultState) {
	r.state, r.done = state, true
}

// finishRound closes the agent's current step: a step that mixed transfer
// with other calls never ran, and the results of the others are recorded in
// the event list.
func (t *tracker) finishRound(rp *replyPath) {
	if rp.path == "" && t.c.toolSet[ToolTransfer] && len(rp.round) > 1 {
		for _, r := range rp.round {
			if r.name == ToolTransfer {
				for _, x := range rp.round {
					x.rejected = true
				}
				break
			}
		}
	}
	for _, r := range rp.round {
		if !r.rejected {
			t.capture(r)
		}
	}
	rp.round = nil
}

func (t *tracker) finishAll() {
	for _, rp := range t.paths {
		t.finishRound(rp)
	}
}

// capture records a finished tool result in the host's event list.
func (t *tracker) capture(r *callRecord) {
	if !t.c.capture || !r.done || (r.state != message.ToolResultSuccess && r.state != message.ToolResultError) {
		return
	}
	if r.path == "" && isThreadTool(r.name) {
		return
	}
	rec := thread.EventRecord{
		Origin: thread.Origin{Participant: t.c.desc.Name, Path: r.path},
		Zone:   t.c.desc.Zone,
		Tool:   r.name,
		Target: thread.ToolTarget(r.args),
		Output: r.output.String(),
	}
	t.host.RecordEvent(rec)
}

// recordUsage writes one ledger entry per model call. A call-end event
// without an open call carries compression usage the agent drained.
func (t *tracker) recordUsage(rp *replyPath, e *event.ModelCallEndEvent) {
	kind := thread.UsageMain
	if !rp.modelOpen {
		kind = thread.UsageCompaction
	}
	rp.modelOpen = false
	t.host.RecordUsage(thread.LedgerEntry{
		Participant: t.c.desc.Name,
		Path:        rp.path,
		Kind:        kind,
		Usage:       usageOf(e),
	})
}

// usageOf converts a model-call-end event. The agent emits zeros when the
// provider reported no usage, so all-zero counts are recorded as unknown.
func usageOf(e *event.ModelCallEndEvent) *model.ChatUsage {
	if e.InputTokens == 0 && e.OutputTokens == 0 && e.CacheCreationTokens == 0 && e.CacheReadTokens == 0 {
		return nil
	}
	return &model.ChatUsage{
		InputTokens:              e.InputTokens,
		OutputTokens:             e.OutputTokens,
		CacheCreationInputTokens: e.CacheCreationTokens,
		CacheInputTokens:         e.CacheReadTokens,
	}
}

// digest returns the digest of the current segment, leaving out the call
// with ID skip (the transfer that hands the segment over) and the main
// agent's update_plan calls, whose plans are the run's progress entries.
func (t *tracker) digest(skip string) *thread.ToolDigest {
	var b thread.DigestBuilder
	for _, r := range t.segment {
		if r.counts() && r.id != skip && (r.path != "" || r.name != planToolName) {
			b.Add(r.displayName(), r.input)
		}
	}
	return b.Digest()
}

// seal starts a new segment after a transfer handed the current one over.
func (t *tracker) seal() {
	t.text.Reset()
	t.segment = nil
}

func (t *tracker) toolCalls() int {
	n := 0
	for _, r := range t.all {
		if r.counts() {
			n++
		}
	}
	return n
}

// writeOf returns the record of a writing call of the main agent, nil for
// any other call.
func (t *tracker) writeOf(rp *replyPath, id string) *writeRecord {
	if rp.path != "" {
		return nil
	}
	return t.writes[id]
}

// eventsSent is the number of Event items the activation has delivered.
func (t *tracker) eventsSent() int {
	if t.sent == nil {
		return 0
	}
	return t.sent()
}

// notes returns one message for each writing call whose answer the agent did
// not record. That happens when the reply was canceled while the call was
// answered: the agent stops waiting and records that no result arrived,
// although the write may have committed. A note for a committed write names
// its entry and carries the entry's seq (thread.SeqMetadataKey), so a peer
// summary that covers the entry later replaces it; a note for a write that
// did not happen says so. Apply them between activations, without moving the
// cursor.
func (t *tracker) notes() []*message.Msg {
	var out []*message.Msg
	for _, id := range t.writeOrder {
		w := t.writes[id]
		if w.delivered {
			continue
		}
		if w.seq == 0 {
			out = append(out, message.UserMsg(noteSender, fmt.Sprintf(
				"[Your %s call was interrupted before its answer was recorded. Its answer: %s]", w.name, w.result.GetOutputText())))
			continue
		}
		m := message.UserMsg(noteSender, fmt.Sprintf(
			"[Your %s call was committed as entry #%d before the reply was interrupted%s.]", w.name, w.seq, w.detail))
		m.Metadata[thread.SeqMetadataKey] = w.seq
		m.Metadata[thread.KindMetadataKey] = string(w.kind)
		out = append(out, m)
	}
	return out
}

// noteSender is the name on the notes of tracker.notes, the sender of
// imported thread entries too.
const noteSender = "thread"

// answerExternal answers an external tool request: the participant's own
// writing tools run here; any other external tool has no answerer inside an
// activation and gets an error result.
func (t *tracker) answerExternal(rp *replyPath, e *event.RequireExternalExecutionEvent) {
	results := make([]message.ToolResultBlock, 0, len(e.ToolCalls))
	for i := range e.ToolCalls {
		call := &e.ToolCalls[i]
		if rp.path == "" && writeTools[call.Name] && t.c.toolSet[call.Name] {
			results = append(results, t.runWrite(call))
			continue
		}
		results = append(results, errorResult(call,
			"external tool "+call.Name+" is not available inside a thread activation; nobody can answer it"))
	}
	if t.submit != nil {
		t.submit(e, results)
	}
}

// end builds the activation's end. waitErr is a team run's error.
func (t *tracker) end(ctxErr, waitErr error) *thread.ActivationEnd {
	end := &thread.ActivationEnd{
		Text:      t.text.String(),
		Digest:    t.digest(""),
		ToolCalls: t.toolCalls(),
	}
	switch {
	case t.mainEnd != nil:
		end.Reason = t.mainEnd.FinishedReason
		if end.Reason == "" {
			end.Reason = types.ReplyCompleted
		}
		switch {
		case end.Reason == types.ReplyError:
			msg := "reply failed"
			if t.mainEnd.Error != nil && t.mainEnd.Error.Message != "" {
				msg = t.mainEnd.Error.Message
			}
			end.Err = errors.New("participant " + t.c.desc.Name + ": " + msg)
		case end.Reason == types.ReplyCompleted && t.partial:
			end.Err = ErrPartialRound
		}
	case ctxErr != nil:
		end.Reason, end.Err = types.ReplyInterrupted, ctxErr
	default:
		end.Reason, end.Err = types.ReplyError, errNoTerminal
	}
	if end.Err == nil && waitErr != nil {
		end.Err = waitErr
		if end.Reason == types.ReplyCompleted || end.Reason == types.ReplyHandedOff {
			end.Reason = types.ReplyError
		}
	}
	if end.Err == nil && t.c.prepared && t.sawModel && t.act.boundaryCount() == 0 {
		end.Err = errors.New("participant " + t.c.desc.Name +
			": the agent was built without Prepared.Options() (no boundary input ran)")
	}
	return end
}

func (a *activation) boundaryCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.boundaryCalls
}

func errorResult(call *message.ToolCallBlock, text string) message.ToolResultBlock {
	return message.ToolResultBlock{Type: "tool_result", ID: call.ID, Name: call.Name, Output: text, State: message.ToolResultError}
}

func successResult(call *message.ToolCallBlock, text string) message.ToolResultBlock {
	return message.ToolResultBlock{Type: "tool_result", ID: call.ID, Name: call.Name, Output: text, State: message.ToolResultSuccess}
}

// cutBytes returns the longest prefix of s within n bytes that ends on a
// rune boundary.
func cutBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// eventBuffer is the capacity of an activation's event channel.
const eventBuffer = 64

// submitTimeout bounds the delivery of an answer to the agent (an external
// result or a confirmation). The agent takes it at once unless a stale value
// still fills its one-value channel while a reply runs; the bound keeps such
// a value from blocking the forwarder or the host.
const submitTimeout = 2 * time.Second

// sender delivers an activation's items. Events are sent while the caller's
// context is live; after it is canceled they are sent only when there is
// room. The End item is always delivered: when the buffer is full after a
// cancellation the oldest buffered item is discarded to make room, so the
// forwarder never blocks on a consumer that stopped reading and never leaks.
// sent counts the Event items placed on the channel; only the forwarder
// goroutine sends and reads it.
type sender struct {
	ctx  context.Context
	out  chan thread.ActivationEvent
	dead bool
	sent int
}

func (s *sender) send(item thread.ActivationEvent) { //nolint:gocritic // items are small and passed like channel values
	if !s.dead {
		select {
		case s.out <- item:
			s.sent++
			return
		case <-s.ctx.Done():
			s.dead = true
		}
	}
	select {
	case s.out <- item:
		s.sent++
	default:
	}
}

func (s *sender) end(end *thread.ActivationEnd) {
	item := thread.ActivationEvent{End: end}
	if !s.dead {
		select {
		case s.out <- item:
			close(s.out)
			return
		case <-s.ctx.Done():
		}
	}
	// Only this goroutine sends, so once an item left the buffer (taken by
	// the consumer or discarded here) the send below has room.
	for {
		select {
		case s.out <- item:
			close(s.out)
			return
		default:
		}
		select {
		case <-s.out:
		default:
		}
	}
}

// endNow returns a channel holding only an End item, for an activation that
// failed before its reply started.
func endNow(reason types.ReplyFinishedReason, err error) <-chan thread.ActivationEvent {
	out := make(chan thread.ActivationEvent, 1)
	out <- thread.ActivationEvent{End: &thread.ActivationEnd{Reason: reason, Err: err}}
	close(out)
	return out
}
