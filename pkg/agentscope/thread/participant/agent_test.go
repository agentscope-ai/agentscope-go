package participant

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

func toolNames(ts []tool.Tool) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

func TestPrepareSelectsToolsAndBindsOnce(t *testing.T) {
	cloud, err := Prepare(cloudDesc, PrepareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(cloud.Tools()); !slices.Equal(got, []string{ToolTransfer, ToolAssignTask, ToolReport, ToolPublishArtifact, ToolThreadRead}) {
		t.Fatalf("cloud tools = %v; event_read is opt-in", got)
	}
	for _, d := range []thread.Descriptor{cloudDesc, edgeDesc} {
		c := &core{desc: d}
		c.configure(nil, 0)
		if c.capture {
			t.Fatalf("%s: tool output capture is opt-in", d.Name)
		}
	}
	edge, err := Prepare(edgeDesc, PrepareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(edge.Tools()); slices.Contains(got, ToolPublishArtifact) || slices.Contains(got, ToolEventRead) || len(got) != len(allToolNames)-2 {
		t.Fatalf("edge tools = %v", got)
	}
	if only, err := Prepare(edgeDesc, PrepareOptions{Tools: []string{ToolThreadRead}}); err != nil || !slices.Equal(toolNames(only.Tools()), []string{ToolThreadRead}) {
		t.Fatalf("explicit tool selection: %v", err)
	}
	if _, err := Prepare(cloudDesc, PrepareOptions{Tools: []string{"nope"}}); err == nil {
		t.Fatal("an unknown tool must be rejected")
	}
	if _, err := Prepare(thread.Descriptor{Name: "a/b", Zone: thread.ZoneCloud}, PrepareOptions{}); err == nil {
		t.Fatal("an invalid descriptor must be rejected")
	}

	for _, tl := range cloud.Tools() {
		write := writeTools[tl.Name()]
		if tl.IsExternalTool() != write || tl.IsReadOnly() == write || tl.IsConcurrencySafe() == write {
			t.Fatalf("%s: external=%v readOnly=%v safe=%v", tl.Name(), tl.IsExternalTool(), tl.IsReadOnly(), tl.IsConcurrencySafe())
		}
		var schema map[string]any
		if err := json.Unmarshal(tl.InputSchema(), &schema); err != nil || schema["additionalProperties"] != false {
			t.Fatalf("%s schema: %v %v", tl.Name(), err, schema)
		}
		if d := tl.CheckPermissions(nil, nil); d.Behavior != permission.BehaviorAllow {
			t.Fatalf("%s permission: %+v", tl.Name(), d)
		}
		if tl.Description() == "" {
			t.Fatalf("%s has no description", tl.Name())
		}
	}

	m := &scriptModel{respond: steps()}
	noPerm := agent.NewUnifiedAgent("cloud", "sys", m, cloud.Options()...)
	if _, err := cloud.Participant(AgentConfig{Descriptor: cloudDesc, Agent: noPerm}); err == nil || !strings.Contains(err.Error(), "PermissionContext") {
		t.Fatalf("a writing tool needs a permission context: %v", err)
	}
	withPerm := agent.NewUnifiedAgent("cloud", "sys", m, append([]agent.AgentOption{agent.WithPermissionContext(permission.NewContext(permission.ModeBypass))}, cloud.Options()...)...)
	if _, err := cloud.Participant(AgentConfig{Descriptor: edgeDesc, Agent: withPerm}); err == nil {
		t.Fatal("a descriptor other than the prepared one must be rejected")
	}
	if _, err := cloud.Participant(AgentConfig{Descriptor: cloudDesc}); err == nil {
		t.Fatal("an agent is required")
	}
	p, err := cloud.Participant(AgentConfig{Descriptor: cloudDesc, Agent: withPerm})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cloud.Participant(AgentConfig{Descriptor: cloudDesc, Agent: withPerm}); err == nil {
		t.Fatal("a second Participant call must fail")
	}
	if p.Descriptor().Name != "cloud" {
		t.Fatalf("descriptor = %+v", p.Descriptor())
	}
	if _, err := p.Activate(context.Background(), foreground(1, "hi")); !errors.Is(err, ErrNotBound) {
		t.Fatalf("Activate before Bind: %v", err)
	}
	if err := p.Bind(nil); err == nil {
		t.Fatal("a nil host must be rejected")
	}
	h := newFakeHost(cloudDesc, edgeDesc)
	if err := p.Bind(h); err != nil {
		t.Fatal(err)
	}
	if err := p.Bind(h); err == nil {
		t.Fatal("a second Bind must fail")
	}
	// Releasing a host that is not the bound one changes nothing; releasing
	// the bound one lets a later Coordinator bind the participant again.
	other := newFakeHost(cloudDesc, edgeDesc)
	p.Release(other)
	if err := p.Bind(other); err == nil {
		t.Fatal("Bind after releasing another host must still fail")
	}
	p.Release(h)
	if err := p.Bind(other); err != nil {
		t.Fatalf("Bind after Release: %v", err)
	}
	if _, err := NewAgentParticipant(AgentConfig{Descriptor: cloudDesc}); err == nil {
		t.Fatal("NewAgentParticipant needs an agent")
	}
}

// TestDigestLeavesOutThePlan: an update_plan call becomes the run's progress
// entry, so the reply's digest lists only the other tools.
func TestDigestLeavesOutThePlan(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		tools: []tool.Tool{textTool("read_file", "package main")},
		agent: []agent.AgentOption{agent.WithPlanTool()},
		respond: steps(
			resp("", call("p1", "update_plan", map[string]any{"plan": []any{map[string]any{"step": "read a.go", "status": "in_progress"}}})),
			resp("", call("c1", "read_file", map[string]any{"path": "a.go"})),
			resp("Done."),
		),
	})
	_, end := activate(t, f.p, foreground(5, "check a.go"), nil)
	if end.Err != nil || end.ToolCalls != 2 || end.Digest.Render() != `read_file {"path":"a.go"}` {
		t.Fatalf("calls=%d digest=%q err=%v", end.ToolCalls, end.Digest.Render(), end.Err)
	}
}

func TestActivateForwardsEventsTextDigestUsageAndEventList(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		tools: []tool.Tool{textTool("read_file", "package main")},
		respond: steps(
			resp("Looking.", call("c1", "read_file", map[string]any{"path": "a.go"})),
			resp("Done."),
		),
	})
	items, end := activate(t, f.p, foreground(5, "check a.go"), nil)
	if end.Reason != types.ReplyCompleted || end.Err != nil {
		t.Fatalf("end: %+v", end)
	}
	if end.Text != "Looking.\nDone." {
		t.Fatalf("text = %q", end.Text)
	}
	if end.ToolCalls != 1 || end.Digest.Render() != `read_file {"path":"a.go"}` {
		t.Fatalf("calls=%d digest=%q", end.ToolCalls, end.Digest.Render())
	}
	if _, ok := items[0].Event.(event.ReplyStartEvent); !ok {
		t.Fatalf("first event = %T", items[0].Event)
	}
	if len(eventsOf[event.ReplyEndEvent](items)) != 1 || len(eventsOf[event.ToolResultEndEvent](items)) != 1 {
		t.Fatal("the reply's events must be forwarded")
	}
	for _, it := range items {
		if it.Path != "" {
			t.Fatalf("main agent event with path %q", it.Path)
		}
	}
	st := f.p.State()
	if st.Cursor != 5 || st.Busy || st.InputTokens == 0 {
		t.Fatalf("state = %+v", st)
	}
	h := f.host.snapshot()
	if len(h.usage) != 2 {
		t.Fatalf("usage = %+v", h.usage)
	}
	for _, u := range h.usage {
		if u.Participant != "cloud" || u.Path != "" || u.Kind != thread.UsageMain || u.Usage == nil || u.Usage.InputTokens != 20 || u.Usage.CacheInputTokens != 4 {
			t.Fatalf("usage entry = %+v %+v", u, u.Usage)
		}
	}
	if len(h.recorded) != 1 {
		t.Fatalf("recorded = %+v", h.recorded)
	}
	r := h.recorded[0]
	if r.Tool != "read_file" || r.Target != "a.go" || r.Output != "package main" || r.Zone != thread.ZoneCloud || r.Origin != (thread.Origin{Participant: "cloud"}) {
		t.Fatalf("record = %+v", r)
	}
}

func TestActivateStreamingTextAndUnknownUsage(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		agent: []agent.AgentOption{agent.WithModelStreaming(true)},
		respond: steps(
			&model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "first"}, message.TextBlock{Type: "text", Text: "second"}}},
		),
	})
	_, end := activate(t, f.p, foreground(2, "hi"), nil)
	if end.Reason != types.ReplyCompleted || !strings.Contains(end.Text, "first") || !strings.Contains(end.Text, "second") {
		t.Fatalf("end: %+v", end)
	}
	h := f.host.snapshot()
	if len(h.usage) != 1 || h.usage[0].Usage != nil {
		t.Fatalf("a call without reported usage is recorded as unknown: %+v", h.usage)
	}
}

func TestActivateAppliesSyncFirstAndFailsOnBadSync(t *testing.T) {
	f := newFixture(t, fixtureOptions{respond: steps(textResp("ok"))})
	act := foreground(6, "hi")
	act.Sync = &thread.Sync{Messages: []*message.Msg{projected(4, "an earlier report")}, Cursor: 4}
	_, end := activate(t, f.p, act, nil)
	if end.Err != nil || !strings.Contains(f.m.input(0), "an earlier report") {
		t.Fatalf("the activation sync must be applied before the reply: %+v %q", end, f.m.input(0))
	}
	if v := f.a.ThreadView(); v.Cursor != 6 {
		t.Fatalf("cursor = %d", v.Cursor)
	}

	bad := foreground(7, "hi")
	bad.Sync = &thread.Sync{Cursor: 2} // behind the cursor
	if ch, err := f.p.Activate(t.Context(), bad); ch != nil || err == nil || f.m.calls() != 1 {
		t.Fatalf("a failed sync fails Activate before the reply: %v calls=%d", err, f.m.calls())
	}
	if f.p.State().Busy {
		t.Fatal("the participant must be idle again")
	}
}

func TestActivateEndsOnceOnCancel(t *testing.T) {
	started := make(chan struct{})
	f := newFixture(t, fixtureOptions{respond: func(ctx context.Context, i int, _ *model.CallOptions) (*model.ChatResponse, error) {
		if i == 0 {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return textResp("again"), nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := f.p.Activate(ctx, foreground(1, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	_, end := drain(t, ch, nil)
	if end.Reason != types.ReplyInterrupted {
		t.Fatalf("end: %+v", end)
	}
	if f.p.State().Busy {
		t.Fatal("the participant must be idle after a canceled activation")
	}
	if _, end := activate(t, f.p, foreground(2, "again"), nil); end.Text != "again" {
		t.Fatalf("a later activation must run: %+v", end)
	}
}

func TestSenderDeliversEndWhenFullAndCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := make(chan thread.ActivationEvent, 2)
	s := &sender{ctx: ctx, out: out}
	for range 3 {
		s.send(thread.ActivationEvent{Event: event.NewCustomEvent("r", "x", nil)})
	}
	s.end(&thread.ActivationEnd{Reason: types.ReplyInterrupted})
	var items []thread.ActivationEvent
	for it := range out {
		items = append(items, it)
	}
	if len(items) == 0 || items[len(items)-1].End == nil || len(items) > 2 {
		t.Fatalf("items = %+v", items)
	}

	live := make(chan thread.ActivationEvent, 4)
	s = &sender{ctx: context.Background(), out: live}
	s.send(thread.ActivationEvent{Event: event.NewCustomEvent("r", "x", nil)})
	s.end(&thread.ActivationEnd{})
	if n := len(live); n != 2 {
		t.Fatalf("live delivery: %d items", n)
	}
}

func TestBusyParticipantRefusesWork(t *testing.T) {
	release := make(chan struct{})
	f := newFixture(t, fixtureOptions{respond: func(ctx context.Context, _ int, _ *model.CallOptions) (*model.ChatResponse, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return textResp("done"), nil
	}})
	ch, err := f.p.Activate(context.Background(), foreground(1, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	if !f.p.State().Busy {
		t.Fatal("State must report busy")
	}
	if _, err := f.p.Activate(context.Background(), foreground(2, "again")); !errors.Is(err, ErrBusy) {
		t.Fatalf("second activation: %v", err)
	}
	if err := f.p.Sync(context.Background(), thread.Sync{Reset: true}); !errors.Is(err, ErrBusy) {
		t.Fatalf("a reset while busy must be refused: %v", err)
	}
	if _, err := f.p.Compact(context.Background(), ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("compact while busy: %v", err)
	}
	// An own summary may be offered while busy; only the snapshot is checked.
	if err := f.p.OfferOwnSummary(context.Background(), nil, "x"); !errors.Is(err, thread.ErrNoSnapshot) {
		t.Fatalf("offer while busy: %v", err)
	}
	close(release)
	if _, end := drain(t, ch, nil); end.Text != "done" {
		t.Fatalf("end: %+v", end)
	}
}

func transferArgs(to string) map[string]any {
	return map[string]any{"to": to, "goal": "Turn on the lamp", "done": []any{"found the lamp"},
		"remaining": []any{"switch it on"}, "constraints": []any{"ask first"}, "refs": []any{"file://plan.md"}}
}

func TestTransferSealsTextAndEndsHandedOff(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		tools: []tool.Tool{textTool("read_file", "lamp: off")},
		respond: steps(
			resp("Reading.", call("c1", "read_file", map[string]any{"path": "lamp.cfg"})),
			resp("Handing over.", call("c2", ToolTransfer, transferArgs("edge"))),
			textResp("must not be called"),
		),
	})
	asks := 0
	items, end := activate(t, f.p, foreground(5, "turn on the lamp"), confirmAll(t, f.p, &asks))
	if end.Reason != types.ReplyHandedOff || end.Err != nil {
		t.Fatalf("end: %+v", end)
	}
	if f.m.calls() != 2 || asks != 0 {
		t.Fatalf("model calls = %d asks = %d", f.m.calls(), asks)
	}
	if end.Text != "" || end.Digest != nil || end.ToolCalls != 2 {
		t.Fatalf("after a transfer End holds only what came later: %+v", end)
	}
	h := f.host.snapshot()
	if len(h.transfers) != 1 {
		t.Fatalf("transfers = %+v", h.transfers)
	}
	req := h.transfers[0]
	if req.From != (thread.Origin{Participant: "cloud"}) || req.Epoch != 3 || req.To != "edge" {
		t.Fatalf("request = %+v", req)
	}
	if req.Text != "Reading.\nHanding over." || req.Digest.Render() != `read_file {"path":"lamp.cfg"}` {
		t.Fatalf("sealed text %q digest %q", req.Text, req.Digest.Render())
	}
	b := req.Brief
	if b.Goal != "Turn on the lamp" || !slices.Equal(b.Remaining, []string{"switch it on"}) || !slices.Equal(b.Done, []string{"found the lamp"}) ||
		!slices.Equal(b.Constraints, []string{"ask first"}) || len(b.Refs) != 1 || b.Refs[0].URI != "file://plan.md" {
		t.Fatalf("brief = %+v", b)
	}
	if len(eventsOf[event.RequireExternalExecutionEvent](items)) != 0 {
		t.Fatal("the participant answers its own external tools; the request is not forwarded")
	}
	var result string
	for _, d := range eventsOf[event.ToolResultTextDeltaEvent](items) {
		result += d.Delta
	}
	if !strings.Contains(result, "Transferred to edge (entry #101). Stop here; edge continues with the user.") {
		t.Fatalf("transfer result = %q", result)
	}
	// Only the second round's text was sealed; recorded events keep the read.
	if len(h.recorded) != 1 || h.recorded[0].Tool != "read_file" {
		t.Fatalf("recorded = %+v", h.recorded)
	}
}

func TestTransferErrorsAreToolResults(t *testing.T) {
	long := strings.Repeat("x", 2500)
	for _, tc := range []struct {
		name     string
		args     map[string]any
		bg       bool
		hostErr  error
		want     string
		attempts int
	}{
		{name: "background", args: transferArgs("edge"), bg: true, want: "only works in a foreground activation"},
		{name: "brief too long", args: map[string]any{"to": "edge", "goal": long, "remaining": []any{"x"}}, want: "the receiver accepts at most 2000"},
		{name: "no remaining", args: map[string]any{"to": "edge", "goal": "g", "remaining": []any{}}, want: "needs the remaining work"},
		{name: "unknown", args: transferArgs("phone"), want: "unknown participant phone; participants: edge"},
		{name: "self", args: transferArgs("cloud"), want: "already hold the foreground"},
		{name: "no target", args: map[string]any{"goal": "g", "remaining": []any{"x"}}, want: `missing required field "to"`},
		{name: "empty target", args: map[string]any{"to": "", "goal": "g", "remaining": []any{"x"}}, want: "transfer needs to"},
		{name: "not owner", args: transferArgs("edge"), hostErr: thread.ErrNotOwner, want: "the foreground already moved", attempts: 1},
		{name: "host error", args: transferArgs("edge"), hostErr: errors.New("store down"), want: "transfer failed: store down", attempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOptions{respond: steps(
				resp("Trying.", call("c1", ToolTransfer, tc.args)),
				textResp("ok"),
			)})
			f.host.writeErr = tc.hostErr
			act := foreground(1, "hi")
			act.Foreground = !tc.bg
			_, end := activate(t, f.p, act, nil)
			if end.Reason != types.ReplyCompleted || end.Text != "Trying.\nok" {
				t.Fatalf("a refused transfer continues the reply: %+v", end)
			}
			if !strings.Contains(f.m.input(1), tc.want) {
				t.Fatalf("model saw %q, want %q", f.m.input(1), tc.want)
			}
			if got := len(f.host.snapshot().transfers); got != tc.attempts {
				t.Fatalf("host transfers = %d, want %d", got, tc.attempts)
			}
		})
	}
}

func TestMixedTransferStepIsNotCounted(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		tools: []tool.Tool{textTool("read_file", "x")},
		respond: steps(
			resp("", call("c1", "read_file", map[string]any{"path": "a"}), call("c2", ToolTransfer, transferArgs("edge"))),
			textResp("ok"),
		),
	})
	_, end := activate(t, f.p, foreground(1, "hi"), nil)
	if end.Reason != types.ReplyCompleted || end.ToolCalls != 0 || end.Digest != nil {
		t.Fatalf("a refused mixed step ran nothing: %+v", end)
	}
	if !strings.Contains(f.m.input(1), "transfer must be called alone") {
		t.Fatalf("model input = %q", f.m.input(1))
	}
	h := f.host.snapshot()
	if len(h.transfers) != 0 || len(h.recorded) != 0 {
		t.Fatalf("transfers=%v recorded=%v", h.transfers, h.recorded)
	}
}

func TestEventCaptureCapAndSwitch(t *testing.T) {
	failing := tool.NewFunctionTool("broken", "fails", objectSchema, func(context.Context, map[string]any) (any, error) {
		return nil, errors.New("disk on fire")
	})
	f := newFixture(t, fixtureOptions{
		cfg:   AgentConfig{MaxEventOutput: 10},
		tools: []tool.Tool{textTool("big", "0123456789abcdef"), failing},
		respond: steps(
			resp("", call("c1", "big", map[string]any{"path": "p"}), call("c2", "broken", nil), call("c3", ToolReport, map[string]any{"text": "r"})),
			textResp("ok"),
		),
	})
	activate(t, f.p, foreground(1, "go"), nil)
	rec := f.host.snapshot().recorded
	if len(rec) != 2 || rec[0].Output != "0123456789" || rec[1].Tool != "broken" || rec[1].Output != "disk on fi" {
		t.Fatalf("recorded = %+v", rec)
	}

	off := false
	g := newFixture(t, fixtureOptions{
		cfg:     AgentConfig{CaptureOutputs: &off},
		tools:   []tool.Tool{textTool("big", "x")},
		respond: steps(resp("", call("c1", "big", nil)), textResp("ok")),
	})
	activate(t, g.p, foreground(1, "go"), nil)
	if n := len(g.host.snapshot().recorded); n != 0 {
		t.Fatalf("capture off recorded %d", n)
	}
}

func TestSyncConvertsPeerSummaryAndCoverage(t *testing.T) {
	f := newFixture(t, fixtureOptions{respond: steps()})
	ctx := context.Background()
	msgs := []*message.Msg{projected(1, "one"), projected(2, "two"), projected(3, "three"), projected(4, "four")}
	if err := f.p.Sync(ctx, thread.Sync{Messages: msgs, Cursor: 4}); err != nil {
		t.Fatal(err)
	}
	if v := f.a.ThreadView(); v.Cursor != 4 || v.ContextMessages != 4 {
		t.Fatalf("view = %+v", v)
	}
	summary := thread.Entry{Seq: 9, Kind: thread.KindSummary, Origin: thread.Origin{Participant: "cloud", Path: "master"},
		Coverage: &thread.Coverage{Through: 3, Excluded: []uint64{2}},
		Content:  []message.ContentBlock{message.TextBlock{Type: "text", Text: "summary text"}}}
	if err := f.p.Sync(ctx, thread.Sync{PeerSummary: &summary, Cursor: 9}); err != nil {
		t.Fatal(err)
	}
	summary.Coverage.Excluded[0] = 99 // the participant keeps its own copy
	v := f.a.ThreadView()
	ps := v.PeerSummary
	if ps == nil || ps.Text != "summary text" || ps.Origin != "cloud/master" || ps.Through != 3 || !slices.Equal(ps.Excluded, []uint64{2}) {
		t.Fatalf("peer summary = %+v", ps)
	}
	if v.Cursor != 9 || v.ContextMessages != 2 {
		t.Fatalf("view = %+v", v)
	}
	if st := f.p.State(); st.Cursor != 9 || st.Busy {
		t.Fatalf("state = %+v", st)
	}
	if err := f.p.Sync(ctx, thread.Sync{Cursor: 5}); err == nil {
		t.Fatal("a cursor moving back must be refused")
	}
	if err := f.p.Sync(ctx, thread.Sync{Reset: true}); err != nil || f.a.ThreadView().ContextMessages != 0 {
		t.Fatalf("a reset between activations clears the context: %v", err)
	}
}

const summaryJSON = `{"task_overview":"overview-marker","current_state":"s","important_discoveries":"d","next_steps":"n","context_to_preserve":"c"}`

func hasTool(o *model.CallOptions, name string) bool {
	for _, s := range o.Tools {
		if s.Function.Name == name {
			return true
		}
	}
	return false
}

func TestCompactUsageArrivesWithNextActivation(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		agent: []agent.AgentOption{agent.WithContextConfig(&agent.ContextConfig{ContextSize: 100000})},
		respond: func(_ context.Context, _ int, o *model.CallOptions) (*model.ChatResponse, error) {
			if hasTool(o, "generate_structured_output") {
				return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "s", Name: "generate_structured_output", Input: summaryJSON}},
					Usage: &model.ChatUsage{InputTokens: 40, OutputTokens: 9}}, nil
			}
			return textResp("answer"), nil
		},
	})
	activate(t, f.p, foreground(1, "first question"), nil)
	changed, err := f.p.Compact(context.Background(), "Summarize strictly.")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	before := len(f.host.snapshot().usage)
	activate(t, f.p, foreground(2, "second question"), nil)
	var compaction []thread.LedgerEntry
	for _, u := range f.host.snapshot().usage[before:] {
		if u.Kind == thread.UsageCompaction {
			compaction = append(compaction, u)
		}
	}
	if len(compaction) != 1 || compaction[0].Usage == nil || compaction[0].Usage.InputTokens != 40 {
		t.Fatalf("compaction usage = %+v", compaction)
	}

	g := newFixture(t, fixtureOptions{respond: steps()})
	if _, err := g.p.Compact(context.Background(), ""); err == nil {
		t.Fatal("compaction without a context config must fail")
	}
}

func TestForkerSnapshotForkAndApply(t *testing.T) {
	f := newFixture(t, fixtureOptions{respond: func(_ context.Context, _ int, o *model.CallOptions) (*model.ChatResponse, error) {
		if o.ToolChoice != nil && o.ToolChoice.Mode == "none" {
			return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "the summary"}},
				Usage: &model.ChatUsage{InputTokens: 50, OutputTokens: 6, CacheInputTokens: 45}}, nil
		}
		return textResp("answer"), nil
	}})
	if _, err := f.p.Snapshot(); !errors.Is(err, thread.ErrNoSnapshot) {
		t.Fatalf("no request captured yet: %v", err)
	}
	activate(t, f.p, foreground(4, "question"), nil)
	snap, err := f.p.Snapshot()
	if err != nil || snap.Cursor() != 4 {
		t.Fatalf("snapshot: %v", err)
	}
	usage := len(f.host.snapshot().usage)
	out, err := f.p.Fork(context.Background(), snap, thread.ForkSpec{Prompt: "Summarize for the edge."})
	if err != nil || out.Text != "the summary" || out.Calls != 1 || out.Usage == nil || out.Usage.CacheInputTokens != 45 {
		t.Fatalf("fork = %+v %v", out, err)
	}
	if len(f.host.snapshot().usage) != usage {
		t.Fatal("the participant must not record fork usage")
	}
	if _, err := f.p.Fork(context.Background(), snap, thread.ForkSpec{}); err == nil {
		t.Fatal("an empty prompt must fail")
	}
	// An offer changes nothing until the compaction line, so a newer offer
	// on the same snapshot replaces it.
	if err := f.p.OfferOwnSummary(context.Background(), snap, "own summary"); err != nil {
		t.Fatal(err)
	}
	if err := f.p.OfferOwnSummary(context.Background(), snap, "newer"); err != nil {
		t.Fatalf("a newer offer: %v", err)
	}
	if _, err := f.p.Fork(context.Background(), foreignSnap{}, thread.ForkSpec{Prompt: "x"}); !errors.Is(err, thread.ErrNoSnapshot) {
		t.Fatalf("foreign snapshot: %v", err)
	}
	if err := f.p.OfferOwnSummary(context.Background(), foreignSnap{}, "x"); err == nil {
		t.Fatal("a foreign snapshot must be refused")
	}
	// A reset of the context leaves the snapshot nothing to match.
	if err := f.p.Sync(context.Background(), thread.Sync{Reset: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.p.OfferOwnSummary(context.Background(), snap, "late"); !errors.Is(err, thread.ErrStale) {
		t.Fatalf("an offer on a stale snapshot: %v", err)
	}

	e := newFixture(t, fixtureOptions{desc: edgeDesc, respond: steps(textResp("ok"))})
	activate(t, e.p, foreground(1, "hi"), nil)
	if _, err := e.p.Snapshot(); !errors.Is(err, thread.ErrNoSnapshot) {
		t.Fatalf("the edge has no fork capture: %v", err)
	}
}

type foreignSnap struct{}

func (foreignSnap) Cursor() uint64 { return 0 }

func TestBoundaryInjectsPendingReport(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		tools: []tool.Tool{textTool("read_file", "x")},
		respond: steps(
			resp("", call("c1", "read_file", map[string]any{"path": "a"})),
			textResp("ok"),
		),
	})
	var cursors []uint64
	f.host.pending = func(call int, cursor uint64) (*thread.Sync, error) {
		cursors = append(cursors, cursor)
		if call == 2 {
			return &thread.Sync{Messages: []*message.Msg{projected(7, "[#7 report from edge] the lamp is on")}, Cursor: 7}, nil
		}
		return nil, nil
	}
	items, end := activate(t, f.p, foreground(3, "hi"), nil)
	if end.Err != nil || strings.Contains(f.m.input(0), "lamp is on") || !strings.Contains(f.m.input(1), "lamp is on") {
		t.Fatalf("the pending report must reach the second model call: %+v", end)
	}
	injected := false
	for _, c := range eventsOf[event.CustomEvent](items) {
		injected = injected || c.Name == agent.ThreadEventInputInjected
	}
	if !injected || f.a.ThreadView().Cursor != 7 {
		t.Fatalf("injected=%v cursor=%d", injected, f.a.ThreadView().Cursor)
	}
	if len(cursors) < 2 || cursors[0] != 3 || cursors[1] != 3 {
		t.Fatalf("pending cursors = %v", cursors)
	}
}

func TestBoundaryFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending func(int, uint64) (*thread.Sync, error)
		want    string
	}{
		{name: "reset", pending: func(int, uint64) (*thread.Sync, error) { return &thread.Sync{Reset: true}, nil }, want: "reset"},
		{name: "host error", pending: func(int, uint64) (*thread.Sync, error) { return nil, errors.New("log offline") }, want: "log offline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOptions{respond: steps(textResp("ok"))})
			f.host.pending = tc.pending
			_, end := activate(t, f.p, foreground(1, "hi"), nil)
			if end.Reason != types.ReplyError || end.Err == nil || !strings.Contains(end.Err.Error(), tc.want) {
				t.Fatalf("end: %+v", end)
			}
		})
	}
}

type nopHook struct{}

func (nopHook) BeforeModelCall(protocol.LoopState, int)                       {}
func (nopHook) AfterModelCall(protocol.LoopState, int, error)                 {}
func (nopHook) BeforeToolExec(protocol.LoopState, int, string)                {}
func (nopHook) AfterToolExec(protocol.LoopState, int, string, error)          {}
func (nopHook) OnStateTransition(protocol.LoopState, protocol.LoopState, int) {}
func (nopHook) OnLoopStart()                                                  {}
func (nopHook) OnLoopEnd(error)                                               {}

func TestMisbuiltAgentFailsLoudly(t *testing.T) {
	// A later WithLoopHooks drops the participant's hook. The fixture only
	// supplies the model and the host.
	g := newFixture(t, fixtureOptions{respond: steps(textResp("ok"))})
	prep, err := Prepare(cloudDesc, PrepareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	opts := append([]agent.AgentOption{agent.WithToolkit(tool.NewToolkit(prep.Tools()...)), agent.WithModelStreaming(false),
		agent.WithPermissionContext(permission.NewContext(permission.ModeBypass))}, prep.Options()...)
	opts = append(opts, agent.WithLoopHooks(nopHook{}))
	a := agent.NewUnifiedAgent("cloud", "sys", g.m, opts...)
	p, err := prep.Participant(AgentConfig{Descriptor: cloudDesc, Agent: a})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Bind(g.host); err != nil {
		t.Fatal(err)
	}
	if _, end := activate(t, p, foreground(1, "hi"), nil); end.Err == nil || !strings.Contains(end.Err.Error(), "loop hook") {
		t.Fatalf("a dropped hook must fail the activation: %+v", end)
	}

	// Host hooks passed through PrepareOptions keep running.
	counted := &countHook{}
	h := newFixture(t, fixtureOptions{prepare: PrepareOptions{LoopHooks: []loop.Hook{counted}}, respond: steps(textResp("ok"))})
	if _, end := activate(t, h.p, foreground(1, "hi"), nil); end.Err != nil || counted.starts != 1 {
		t.Fatalf("end=%+v starts=%d", end, counted.starts)
	}

	// No participant options at all: the boundary never ran.
	n := newFixture(t, fixtureOptions{noOption: true, respond: steps(textResp("ok"))})
	if _, end := activate(t, n.p, foreground(1, "hi"), nil); end.Err == nil || !strings.Contains(end.Err.Error(), "Prepared.Options") {
		t.Fatalf("missing options must fail the activation: %+v", end)
	}
}

type countHook struct {
	nopHook
	starts int
}

func (c *countHook) OnLoopStart() { c.starts++ }

func TestSubmitUserConfirmErrors(t *testing.T) {
	f := newFixture(t, fixtureOptions{respond: steps(textResp("ok"))})
	ok := &event.UserConfirmResultEvent{}
	if err := f.p.SubmitUserConfirm("member", ok); err == nil {
		t.Fatal("an unknown path must fail")
	}
	if err := f.p.SubmitUserConfirm("", nil); err == nil {
		t.Fatal("a nil result must fail")
	}
	if err := f.p.SubmitUserConfirm("", ok); err == nil {
		t.Fatal("no activation is running")
	}
	release := make(chan struct{})
	f.m.respond = func(ctx context.Context, _ int, _ *model.CallOptions) (*model.ChatResponse, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return textResp("done"), nil
	}
	ch, err := f.p.Activate(context.Background(), foreground(1, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.p.SubmitUserConfirm("", ok); err == nil {
		t.Fatal("no confirmation is pending")
	}
	close(release)
	drain(t, ch, nil)
}

// TestActivateRefusesNilContext: a nil context fails Activate before
// anything runs.
func TestActivateRefusesNilContext(t *testing.T) {
	f := newFixture(t, fixtureOptions{respond: steps(textResp("unused"))})
	var ctx context.Context // nil: the case under test
	if ch, err := f.p.Activate(ctx, foreground(1, "hi")); ch != nil || err == nil {
		t.Fatalf("Activate(nil) = %v, %v", ch, err)
	}
	if f.m.calls() != 0 || f.p.State().Busy {
		t.Fatal("nothing may run")
	}
}

// TestRepeatedCallIDWithinReply: Gemini numbers calls per response, so two
// rounds of one reply can use the same call ID. Each call keeps its own
// record in the digest, the call count and the event list.
func TestRepeatedCallIDWithinReply(t *testing.T) {
	const id = "gemini-call-read_file-0"
	f := newFixture(t, fixtureOptions{
		tools: []tool.Tool{textTool("read_file", "package main")},
		respond: steps(
			resp("", call(id, "read_file", map[string]any{"path": "a.go"})),
			resp("", call(id, "read_file", map[string]any{"path": "b.go"})),
			textResp("Done."),
		),
	})
	_, end := activate(t, f.p, foreground(5, "check"), nil)
	want := "read_file {\"path\":\"a.go\"}\nread_file {\"path\":\"b.go\"}"
	if end.Err != nil || end.ToolCalls != 2 || end.Digest.Render() != want {
		t.Fatalf("calls=%d digest=%q err=%v", end.ToolCalls, end.Digest.Render(), end.Err)
	}
	h := f.host.snapshot()
	if len(h.recorded) != 2 || h.recorded[0].Target != "a.go" || h.recorded[1].Target != "b.go" ||
		h.recorded[1].Output != "package main" {
		t.Fatalf("recorded = %+v", h.recorded)
	}
}
