package participant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// toolEnv is a prepared core bound to a fake host with a running activation,
// for calling the thread tools directly.
type toolEnv struct {
	c    *core
	host *fakeHost
	act  *activation
	tr   *tracker
}

func newToolEnv(t *testing.T, d thread.Descriptor) *toolEnv { //nolint:gocritic // test helper
	t.Helper()
	var opts PrepareOptions
	var capture *bool
	optIntoEvents(d, &opts, &capture)
	prep, err := Prepare(d, opts)
	if err != nil {
		t.Fatal(err)
	}
	c := prep.c
	c.configure(nil, 0)
	h := newFakeHost(cloudDesc, edgeDesc)
	if err := c.bind(h); err != nil {
		t.Fatal(err)
	}
	a := newActivation(&thread.Activation{Epoch: 2, Depth: 1, Foreground: true, Trigger: trigger(1, "go")})
	if _, err := c.beginActivation(a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.endActivation)
	return &toolEnv{c: c, host: h, act: a, tr: newTracker(c, a, h)}
}

func (e *toolEnv) tool(name string) tool.Tool {
	for _, t := range e.c.tools {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

func (e *toolEnv) run(t *testing.T, name string, in map[string]any) (string, message.ToolResultState) {
	t.Helper()
	r, err := e.tool(name).Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return respText(r), r.State
}

func (e *toolEnv) write(name string, in map[string]any) message.ToolResultBlock {
	c := call("w1", name, in)
	return e.tr.runWrite(&c)
}

func respText(r *tool.ToolResponse) string {
	var parts []string
	for _, b := range r.Content {
		if tb, ok := b.(message.TextBlock); ok {
			parts = append(parts, tb.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func resultText(r *message.ToolResultBlock) string {
	return r.GetOutputText()
}

func TestThreadToolsOutsideActivation(t *testing.T) {
	prep, err := Prepare(cloudDesc, PrepareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range prep.Tools() {
		r, err := tl.Execute(context.Background(), map[string]any{})
		if err != nil || r.State != message.ToolResultError {
			t.Fatalf("%s outside an activation: %v %+v", tl.Name(), err, r)
		}
		want := "only works while this participant is running"
		if writeTools[tl.Name()] {
			want = "answered by the thread participant"
		}
		if !strings.Contains(respText(r), want) {
			t.Fatalf("%s: %q", tl.Name(), respText(r))
		}
	}
}

func TestAssignReportPublish(t *testing.T) {
	e := newToolEnv(t, cloudDesc)
	r := e.write(ToolAssignTask, map[string]any{"to": "edge", "task": "read the sensor", "notify": true})
	if r.State != message.ToolResultSuccess || resultText(&r) != "Assigned task task-1 to edge (entry #101). The result arrives later as a report and is shown to you at your next step." {
		t.Fatalf("assign: %+v", r)
	}
	r = e.write(ToolAssignTask, map[string]any{"to": "edge", "task": "again"})
	if !strings.HasSuffix(resultText(&r), "read it with thread_read when you need it.") {
		t.Fatalf("assign without notify: %q", resultText(&r))
	}
	r = e.write(ToolReport, map[string]any{"text": "the lamp is on", "to": []any{"edge", " "}, "task_id": "task-1", "notify": true})
	if r.State != message.ToolResultSuccess || resultText(&r) != "Reported (entry #103)." {
		t.Fatalf("report: %+v", r)
	}
	r = e.write(ToolPublishArtifact, map[string]any{"uri": "file://plan.md", "summary": "the plan", "size": 42, "text": "note"})
	if r.State != message.ToolResultSuccess || resultText(&r) != "Published file://plan.md (entry #104)." {
		t.Fatalf("publish: %+v", r)
	}
	h := e.host.snapshot()
	a := h.assigns[0]
	if a.From.Participant != "cloud" || a.Epoch != 2 || a.Depth != 1 || a.To != "edge" || a.Text != "read the sensor" || !a.Notify {
		t.Fatalf("assign request = %+v", a)
	}
	rp := h.reports[0]
	if rp.Text != "the lamp is on" || len(rp.To) != 1 || rp.To[0] != "edge" || rp.TaskID != "task-1" || !rp.Notify {
		t.Fatalf("report request = %+v", rp)
	}
	pb := h.publishes[0]
	if len(pb.Refs) != 1 || pb.Refs[0] != (thread.ArtifactRef{URI: "file://plan.md", Summary: "the plan", Size: 42}) || pb.Text != "note" {
		t.Fatalf("publish request = %+v", pb)
	}
}

func TestWriteToolErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   map[string]any
		host error
		want string
	}{
		{ToolAssignTask, map[string]any{"to": "edge"}, nil, "assign_task needs to"},
		{ToolAssignTask, map[string]any{"to": "phone", "task": "x"}, nil, "unknown participant phone; participants: edge"},
		{ToolAssignTask, map[string]any{"to": "edge", "task": "x"}, errors.New("full"), "assign_task failed: full"},
		{ToolReport, map[string]any{"text": "  "}, nil, "report needs text"},
		{ToolReport, map[string]any{"text": "x"}, errors.New("full"), "report failed: full"},
		{ToolPublishArtifact, map[string]any{}, nil, "publish_artifact needs uri"},
		{ToolPublishArtifact, map[string]any{"uri": "u"}, errors.New("full"), "publish_artifact failed: full"},
	} {
		e := newToolEnv(t, cloudDesc)
		e.host.writeErr = tc.host
		r := e.write(tc.name, tc.in)
		if r.State != message.ToolResultError || !strings.Contains(resultText(&r), tc.want) {
			t.Fatalf("%s %v: %+v", tc.name, tc.in, r)
		}
	}

	e := newToolEnv(t, cloudDesc)
	bad := message.ToolCallBlock{ID: "b", Name: ToolReport, Input: "{not json"}
	if r := e.tr.runWrite(&bad); !strings.Contains(resultText(&r), "not valid JSON") {
		t.Fatalf("bad input: %+v", r)
	}
}

// TestWriteFailuresTellTheModelNothingWasWritten covers the errors a write
// returns when it wrote nothing: an interrupted or closed thread.
func TestWriteFailuresTellTheModelNothingWasWritten(t *testing.T) {
	interrupted := fmt.Errorf("%w: cloud: %w", thread.ErrInterrupted, context.Canceled)
	for _, tc := range []struct {
		name string
		in   map[string]any
		err  error
		want string
	}{
		{ToolTransfer, map[string]any{"to": "edge", "goal": "g", "remaining": []any{"r"}}, interrupted,
			"transfer was not done: the reply was interrupted before the call was recorded, so nothing was written. Call transfer again if it is still needed."},
		{ToolAssignTask, map[string]any{"to": "edge", "task": "x"}, interrupted, "assign_task was not done: the reply was interrupted"},
		{ToolReport, map[string]any{"text": "x"}, thread.ErrClosed, "report was not done: the thread is closed, so nothing was written."},
		{ToolPublishArtifact, map[string]any{"uri": "u"}, interrupted, "publish_artifact was not done"},
		{ToolTransfer, map[string]any{"to": "edge", "goal": "g", "remaining": []any{"r"}}, errors.New("store down"), "transfer failed: store down"},
	} {
		e := newToolEnv(t, cloudDesc)
		e.host.writeErr = tc.err
		r := e.write(tc.name, tc.in)
		if r.State != message.ToolResultError || !strings.Contains(resultText(&r), tc.want) {
			t.Fatalf("%s with %v: %q", tc.name, tc.err, resultText(&r))
		}
		if w := e.tr.writes["w1"]; w == nil || w.seq != 0 {
			t.Fatalf("%s: a write that failed must not count as committed: %+v", tc.name, w)
		}
	}
}

// TestNotesNameUnrecordedWrites covers the notes for writes whose answer the
// agent did not record: a committed one names its entry and carries the seq
// in the message metadata, one that wrote nothing repeats its answer, and a
// write whose answer the agent recorded (its end event matched) gets none.
func TestNotesNameUnrecordedWrites(t *testing.T) {
	e := newToolEnv(t, cloudDesc)
	e.tr.handle(event.ReplyStartEvent{ReplyID: "r1", Name: "cloud"})
	assign := call("a1", ToolAssignTask, map[string]any{"to": "edge", "task": "look"})
	e.tr.runWrite(&assign)
	report := call("p1", ToolReport, map[string]any{"text": "progress"})
	answered := e.tr.runWrite(&report)
	e.host.writeErr = fmt.Errorf("%w: cloud: %w", thread.ErrInterrupted, context.Canceled)
	publish := call("u1", ToolPublishArtifact, map[string]any{"uri": "file://a"})
	e.tr.runWrite(&publish)
	// Only the report's answer reached the agent.
	e.tr.handle(event.ToolResultTextDeltaEvent{ReplyID: "r1", ToolCallID: "p1", Delta: answered.GetOutputText()})
	e.tr.handle(event.ToolResultEndEvent{ReplyID: "r1", ToolCallID: "p1", State: answered.State})
	// The assign's end event carries another output: the agent's own
	// "no result" answer.
	e.tr.handle(event.ToolResultTextDeltaEvent{ReplyID: "r1", ToolCallID: "a1", Delta: "External execution timed out"})
	e.tr.handle(event.ToolResultEndEvent{ReplyID: "r1", ToolCallID: "a1", State: message.ToolResultError})

	notes := e.tr.notes()
	if len(notes) != 2 {
		t.Fatalf("notes = %d", len(notes))
	}
	first := notes[0].GetTextContent("")
	if first == nil || *first != "[Your assign_task call was committed as entry #101 before the reply was interrupted: task task-1 went to edge.]" {
		t.Fatalf("committed note = %v", first)
	}
	if seq, ok := agent.ThreadSeqOf(notes[0]); !ok || seq != 101 || notes[0].Metadata[thread.KindMetadataKey] != string(thread.KindAssign) {
		t.Fatalf("committed note metadata = %+v", notes[0].Metadata)
	}
	second := notes[1].GetTextContent("")
	if second == nil || !strings.HasPrefix(*second, "[Your publish_artifact call was interrupted before its answer was recorded. Its answer: publish_artifact was not done") {
		t.Fatalf("not-done note = %v", second)
	}
	if _, ok := agent.ThreadSeqOf(notes[1]); ok {
		t.Fatal("a write that wrote nothing has no entry to name")
	}
}

func TestAnswerExternalRefusesForeignTools(t *testing.T) {
	e := newToolEnv(t, cloudDesc)
	var got []message.ToolResultBlock
	e.tr.submit = func(_ *event.RequireExternalExecutionEvent, results []message.ToolResultBlock) { got = results }
	req := &event.RequireExternalExecutionEvent{ToolCalls: []message.ToolCallBlock{
		call("x1", "host_tool", nil), call("x2", ToolReport, map[string]any{"text": "hi"}),
	}}
	e.tr.answerExternal(&replyPath{calls: map[string]*callRecord{}}, req)
	if len(got) != 2 || got[0].State != message.ToolResultError || !strings.Contains(resultText(&got[0]), "nobody can answer it") ||
		got[1].State != message.ToolResultSuccess {
		t.Fatalf("results = %+v", got)
	}
	// A member's request for a thread tool is not the participant's own.
	e.tr.answerExternal(&replyPath{path: "member", calls: map[string]*callRecord{}}, req)
	if got[1].State != message.ToolResultError {
		t.Fatalf("member results = %+v", got)
	}
}

func TestThreadRead(t *testing.T) {
	e := newToolEnv(t, cloudDesc)
	summary := thread.Entry{Seq: 4, Kind: thread.KindSummary, Origin: thread.Origin{Participant: "cloud"},
		Summary: &thread.SummaryMeta{Target: "edge"}, Coverage: &thread.Coverage{Through: 3},
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "what happened"}}}
	e.host.entries = []thread.Entry{entryText(2, thread.KindReport, "first report"), entryText(3, thread.KindReport, "the lamp is on"), summary}

	out, st := e.run(t, ToolThreadRead, map[string]any{"after": 2})
	if st != message.ToolResultSuccess || !strings.Contains(out, "the lamp is on") ||
		!strings.Contains(out, "[#4 summary from cloud for edge, covering through #3]\nwhat happened") ||
		strings.Contains(out, "first report") {
		t.Fatalf("thread_read after=2: %q", out)
	}

	out, _ = e.run(t, ToolThreadRead, map[string]any{"limit": 1})
	if !strings.Contains(out, "first report") || !strings.HasSuffix(out, "[Limit reached; more entries may follow: thread_read after=2.]") {
		t.Fatalf("limit: %q", out)
	}
	out, _ = e.run(t, ToolThreadRead, map[string]any{"seqs": []any{4.0, -1.0, 1.5}, "kinds": []any{"summary"}})
	if !strings.HasPrefix(out, "[#4 summary") || strings.Contains(out, "lamp") {
		t.Fatalf("seqs and kinds: %q", out)
	}
	if q := e.host.reads[len(e.host.reads)-1]; len(q.Seqs) != 1 || q.Seqs[0] != 4 || len(q.Kinds) != 1 {
		t.Fatalf("query = %+v", q)
	}
	if out, _ = e.run(t, ToolThreadRead, map[string]any{"after": 99}); out != "No thread entries match." {
		t.Fatalf("empty: %q", out)
	}
	e.host.readErr = errors.New("log offline")
	if out, st = e.run(t, ToolThreadRead, nil); st != message.ToolResultError || !strings.Contains(out, "thread_read failed: log offline") {
		t.Fatalf("read error: %q", out)
	}
}

func TestThreadReadCapsOutput(t *testing.T) {
	e := newToolEnv(t, cloudDesc)
	e.host.entries = []thread.Entry{
		entryText(2, thread.KindReport, strings.Repeat("a", 4000)),
		entryText(3, thread.KindReport, strings.Repeat("b", 4000)),
	}
	out, _ := e.run(t, ToolThreadRead, nil)
	if strings.Contains(out, "bbb") || !strings.HasSuffix(out, "[Output capped at 6000 characters. Continue with thread_read after=2.]") {
		t.Fatalf("cap: %d %q", len(out), out[len(out)-90:])
	}
}

// TestThreadReadReadsTheRestOfACutEntry covers an entry longer than the cap:
// the cut notice names the offset, and offset reads the rest of that one
// entry, which paging with after would skip.
func TestThreadReadReadsTheRestOfACutEntry(t *testing.T) {
	e := newToolEnv(t, cloudDesc)
	long := strings.Repeat("é", 6990) + "THE-END"
	e.host.entries = []thread.Entry{entryText(7, thread.KindReport, long), entryText(8, thread.KindReport, "next entry")}
	whole := e.c.renderForRead(e.host.entries[:1])
	total := len([]rune(whole))

	out, _ := e.run(t, ToolThreadRead, nil)
	body, notice, _ := strings.Cut(out, "\n[Entry #7 cut")
	if n := len([]rune(body)); n != threadReadMaxRunes || strings.Contains(out, "THE-END") || strings.Contains(out, "next entry") {
		t.Fatalf("a single long entry is cut to the cap: %d runes", n)
	}
	if want := " at character 6000. Read the rest with thread_read seqs=[7] offset=6000; later entries with thread_read after=7.]"; notice != want {
		t.Fatalf("cut notice = %q", notice)
	}

	out, st := e.run(t, ToolThreadRead, map[string]any{"seqs": []any{7.0}, "offset": 6000.0})
	rest := strings.TrimPrefix(out, "[#7 from character 6000]\n")
	if st != message.ToolResultSuccess || rest == out || rest != string([]rune(whole)[6000:]) || !strings.HasSuffix(out, "THE-END") {
		t.Fatalf("the rest of #7 (%d of %d characters): %q", len([]rune(rest)), total, out[:min(len(out), 80)])
	}
	if q := e.host.reads[len(e.host.reads)-1]; len(q.Seqs) != 1 || q.Seqs[0] != 7 {
		t.Fatalf("query = %+v", q)
	}
	// With after, the offset applies to the first entry shown only.
	out, _ = e.run(t, ToolThreadRead, map[string]any{"after": 0.0, "offset": 6000.0})
	if !strings.HasPrefix(out, "[#7 from character 6000]\n") || !strings.Contains(out, "THE-END\n") || !strings.HasSuffix(out, "next entry") {
		t.Fatalf("offset with after: %q", out[max(0, len(out)-120):])
	}
	if out, st = e.run(t, ToolThreadRead, map[string]any{"seqs": []any{7.0}, "offset": float64(total)}); st != message.ToolResultError ||
		!strings.Contains(out, "past the end of entry #7") {
		t.Fatalf("offset past the end: %q", out)
	}
}

func TestEventRead(t *testing.T) {
	// An edge reader sees the cloud's records; a zone never sees its own.
	e := newToolEnv(t, edgeDesc)
	if out, _ := e.run(t, ToolEventRead, map[string]any{"action": "outline"}); out != "No tool outputs are available to read." {
		t.Fatalf("empty outline: %q", out)
	}
	e.host.events.Add(thread.EventRecord{Origin: thread.Origin{Participant: "edge"}, Zone: thread.ZoneCloud, Tool: "web_fetch",
		Target: "https://x", Output: "line one\nline two"})
	e.host.events.Add(thread.EventRecord{Origin: thread.Origin{Participant: "cloud", Path: "researcher"}, Zone: thread.ZoneCloud,
		Tool: "read_file", Output: "header\n" + strings.Repeat("z", 293)})

	out, st := e.run(t, ToolEventRead, map[string]any{"action": "search", "query": "read_file"})
	if st != message.ToolResultSuccess || out != "ev-2 read_file (from cloud/researcher, 300 characters): header" {
		t.Fatalf("search: %q", out)
	}
	out, _ = e.run(t, ToolEventRead, map[string]any{"action": "page", "id": "ev-2", "offset": 10, "limit": 100})
	if !strings.HasPrefix(out, "ev-2 characters 10-110 of 300:\n") || !strings.HasSuffix(out, "[More: event_read action=page id=ev-2 offset=110]") {
		t.Fatalf("page: %q", out)
	}
	out, _ = e.run(t, ToolEventRead, map[string]any{"action": "page", "id": "ev-1"})
	if out != "ev-1 characters 0-17 of 17:\nline one\nline two" {
		t.Fatalf("whole page: %q", out)
	}

	f := newToolEnv(t, edgeDesc)
	f.host.events = e.host.events
	out, _ = f.run(t, ToolEventRead, map[string]any{"action": "outline", "limit": 5})
	if out != "ev-1 web_fetch https://x (from edge, 17 characters): line one\n"+
		"ev-2 read_file (from cloud/researcher, 300 characters): header" {
		t.Fatalf("outline: %q", out)
	}

	for _, tc := range []struct {
		in   map[string]any
		want string
	}{
		{map[string]any{"action": "search"}, "event_read search needs query"},
		{map[string]any{"action": "search", "query": "nothing"}, "No tool outputs match nothing."},
		{map[string]any{"action": "page"}, "event_read page needs id"},
		{map[string]any{"action": "page", "id": "ev-9"}, "event_read page ev-9 failed"},
		{map[string]any{"action": "delete"}, "action must be outline, search or page"},
	} {
		if out, _ := f.run(t, ToolEventRead, tc.in); !strings.Contains(out, tc.want) {
			t.Fatalf("%v: %q", tc.in, out)
		}
	}
}

func TestSystemPromptSection(t *testing.T) {
	phone := thread.Descriptor{Name: "phone", Zone: thread.ZoneEdge, Description: "phone agent", Window: 4000}
	a := SystemPromptSection(cloudDesc, []thread.Descriptor{cloudDesc, edgeDesc, phone})
	b := SystemPromptSection(cloudDesc, []thread.Descriptor{phone, edgeDesc, cloudDesc})
	if a == "" || a != b {
		t.Fatalf("the section must be stable:\n%s\n---\n%s", a, b)
	}
	if !strings.HasPrefix(a, "You are participant cloud (zone cloud)") || !strings.Contains(a, thread.RenderPeers("cloud", []thread.Descriptor{edgeDesc, phone})) {
		t.Fatalf("section = %q", a)
	}
	if s := SystemPromptSection(cloudDesc, []thread.Descriptor{cloudDesc}); s != "" {
		t.Fatalf("no peers: %q", s)
	}
}

func TestArgumentHelpers(t *testing.T) {
	in := map[string]any{"f": 7.0, "neg": -2.0, "big": 1e12, "i": 5, "ni": -5, "n": json.Number("9"), "nbad": json.Number("x"), "s": "str"}
	for key, want := range map[string]int{"f": 7, "neg": 0, "big": 0, "i": 5, "ni": 0, "n": 9, "nbad": 0, "s": 0, "missing": 0} {
		if got := intArg(in, key); got != want {
			t.Fatalf("intArg(%s) = %d, want %d", key, got, want)
		}
	}
	for _, tc := range []struct {
		v    any
		want uint64
		ok   bool
	}{
		{3.0, 3, true}, {3.5, 0, false}, {-1.0, 0, false}, {4, 4, true}, {-4, 0, false},
		{json.Number("12"), 12, true}, {json.Number("-1"), 0, false}, {"7", 0, false},
	} {
		if got, ok := seqArg(tc.v); got != tc.want || ok != tc.ok {
			t.Fatalf("seqArg(%v) = %d %v", tc.v, got, ok)
		}
	}
	if !boolArg(map[string]any{"b": true}, "b") || boolArg(map[string]any{"b": "true"}, "b") {
		t.Fatal("boolArg")
	}
	if got := cutBytes("héllo", 2); got != "h" {
		t.Fatalf("cutBytes = %q", got)
	}
	if cutBytes("abc", 0) != "" || cutBytes("abc", 5) != "abc" {
		t.Fatal("cutBytes bounds")
	}
}

func TestTrackerHandlesStrayEvents(t *testing.T) {
	e := newToolEnv(t, cloudDesc)
	// An event of a reply whose start was not seen opens a main path.
	if fwd, path := e.tr.handle(event.NewCustomEvent("r9", "x", nil)); !fwd || path != "" {
		t.Fatalf("stray event: %v %q", fwd, path)
	}
	team := newTracker(e.c, e.act, e.host)
	team.team, team.mainName = true, "master"
	if fwd, path := team.handle(event.NewCustomEvent("r9", "x", nil)); !fwd || path != "" || len(team.paths) != 0 {
		t.Fatalf("team stray event: %v %q", fwd, path)
	}
	// Usage events without an open model call are compaction usage.
	e.tr.handle(event.ModelCallEndEvent{ReplyID: "r9", InputTokens: 3})
	if u := e.host.snapshot().usage; len(u) != 1 || u[0].Kind != thread.UsageCompaction || u[0].Usage.InputTokens != 3 {
		t.Fatalf("usage = %+v", u)
	}
	// A completed reply whose last round was partial keeps its text and
	// reports ErrPartialRound.
	e.tr.handle(event.ModelCallStartEvent{ReplyID: "r9"})
	e.tr.handle(event.TextBlockStartEvent{ReplyID: "r9"})
	e.tr.handle(event.TextBlockDeltaEvent{ReplyID: "r9", Delta: "half"})
	e.tr.handle(event.NewCustomEvent("r9", partialResponseEvent, nil))
	e.tr.handle(event.ReplyEndEvent{ReplyID: "r9"})
	end := e.tr.end(nil, nil)
	if !errors.Is(end.Err, ErrPartialRound) || end.Text != "half" {
		t.Fatalf("end = %+v", end)
	}
	// No terminal event: interrupted on cancel, an error otherwise.
	bare := newTracker(e.c, e.act, e.host)
	if end := bare.end(context.Canceled, nil); !errors.Is(end.Err, context.Canceled) || end.Reason != types.ReplyInterrupted {
		t.Fatalf("canceled = %+v", end)
	}
	if end := bare.end(nil, nil); !errors.Is(end.Err, errNoTerminal) {
		t.Fatalf("no terminal = %+v", end)
	}
	if end := bare.end(nil, errors.New("team failed")); end.Err == nil {
		t.Fatalf("a team error is kept: %+v", end)
	}
}

func TestToUpdateCopies(t *testing.T) {
	s := &thread.Sync{Messages: []*message.Msg{projected(1, "a")}, Cursor: 1}
	u, err := toUpdate(s)
	if err != nil {
		t.Fatal(err)
	}
	s.Messages[0] = nil
	if u.Messages[0] == nil || u.Cursor != 1 || u.PeerSummary != nil {
		t.Fatalf("update = %+v", u)
	}
	var _ agent.ThreadUpdate = u
}

// TestEmptyPeerSummaryFailsTheSync covers a summary entry without text: the
// agent refuses such a summary, so the whole Sync fails with a clear error
// on every path that converts one, and the agent keeps its context.
func TestEmptyPeerSummaryFailsTheSync(t *testing.T) {
	blank := entryText(7, thread.KindSummary, " \n ")
	s := thread.Sync{PeerSummary: &blank, Messages: []*message.Msg{projected(6, "a")}, Cursor: 7}
	if _, err := toUpdate(&s); !errors.Is(err, errEmptySummary) || !strings.Contains(err.Error(), "#7") {
		t.Fatalf("toUpdate: %v", err)
	}

	f := newFixture(t, fixtureOptions{respond: steps(textResp("unused"))})
	if err := f.p.Sync(t.Context(), s); !errors.Is(err, errEmptySummary) {
		t.Fatalf("Sync: %v", err)
	}
	if v := f.a.ThreadView(); v.Cursor != 0 || v.ContextMessages != 0 {
		t.Fatalf("a failed Sync changed the agent: %+v", v)
	}
	// A Sync that cannot be applied fails Activate itself: an End error
	// would tell the Coordinator the Sync was applied.
	if ch, err := f.p.Activate(t.Context(), thread.Activation{Epoch: 1, Foreground: true, Input: "hi", Sync: &s}); ch != nil || !errors.Is(err, errEmptySummary) {
		t.Fatalf("Activate: %v", err)
	}
	if f.p.State().Busy {
		t.Fatal("a refused activation must leave the participant idle")
	}
	// The same summary arriving at a boundary inside the reply ends it.
	f.host.pending = func(int, uint64) (*thread.Sync, error) { return &s, nil }
	_, end := activate(t, f.p, foreground(1, "hi"), nil)
	if end.Reason != types.ReplyError || end.Err == nil || !strings.Contains(end.Err.Error(), "peer summary entry has no text") {
		t.Fatalf("boundary end: %+v", end)
	}
	f.host.pending = nil

	tf := newTeamFixture(t, teamOptions{master: queued(textResp("ok"))})
	if err := tf.tp.Sync(t.Context(), s); !errors.Is(err, errEmptySummary) {
		t.Fatalf("team Sync: %v", err)
	}
	if st := tf.tp.State(); st.Cursor != 0 {
		t.Fatalf("a failed Sync was queued: %+v", st)
	}
	if ch, err := tf.tp.Activate(t.Context(), thread.Activation{Epoch: 1, Foreground: true, Input: "hi", Sync: &s}); ch != nil || !errors.Is(err, errEmptySummary) {
		t.Fatalf("team Activate: %v", err)
	}
	if tf.tp.State().Busy {
		t.Fatal("a refused team activation must leave the participant idle")
	}
}
