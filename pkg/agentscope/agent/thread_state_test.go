package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// seamModel is a scripted model for the thread seams. It records a copy of
// every call's messages and options and answers through respond. Both the
// buffered and the streaming entry points are served.
type seamModel struct {
	mu      sync.Mutex
	calls   []seamCall
	respond func(call int, msgs []*message.Msg, o *model.CallOptions) (*model.ChatResponse, error)
	tokens  func(msgs []*message.Msg) int // nil counts 10 per call
	window  int
}

type seamCall struct {
	msgs   []*message.Msg
	opts   model.CallOptions
	stream bool
}

func (m *seamModel) record(msgs []*message.Msg, opts []model.CallOption, stream bool) (int, model.CallOptions) {
	var o model.CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, seamCall{msgs: cloneMsgs(msgs), opts: o, stream: stream})
	return len(m.calls) - 1, o
}

func (m *seamModel) Chat(_ context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	idx, o := m.record(msgs, opts, false)
	return m.respond(idx, msgs, &o)
}

func (m *seamModel) ChatStream(_ context.Context, msgs []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	idx, o := m.record(msgs, opts, true)
	resp, err := m.respond(idx, msgs, &o)
	if err != nil {
		return nil, err
	}
	ch := make(chan model.ChatResponse, 1)
	final := *resp
	final.IsLast = true
	ch <- final
	close(ch)
	return ch, nil
}

func (m *seamModel) CountTokens(msgs []*message.Msg, _ []model.ToolSchema) int {
	if m.tokens != nil {
		return m.tokens(msgs)
	}
	return 10
}

func (m *seamModel) ContextSize() int { return m.window }

func (m *seamModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *seamModel) call(i int) seamCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[i]
}

func seamText(text string) *model.ChatResponse {
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: text}},
		IsLast:  true,
		Usage:   &model.ChatUsage{InputTokens: 7, OutputTokens: 2},
	}
}

func seamToolCalls(calls ...message.ToolCallBlock) *model.ChatResponse {
	blocks := make([]message.ContentBlock, len(calls))
	for i, c := range calls {
		c.Type = "tool_call"
		if c.Input == "" {
			c.Input = `{}`
		}
		c.State = message.ToolCallPending
		blocks[i] = c
	}
	return &model.ChatResponse{Content: blocks, IsLast: true, Usage: &model.ChatUsage{InputTokens: 7, OutputTokens: 2}}
}

// charTokens counts text bytes plus a fixed cost per message and block.
func charTokens(msgs []*message.Msg) int {
	n := 0
	for _, m := range msgs {
		if m == nil {
			continue
		}
		n += 4
		for _, b := range m.Content {
			switch blk := b.(type) {
			case message.TextBlock:
				n += len(blk.Text)
			case message.ToolResultBlock:
				if s, ok := blk.Output.(string); ok {
					n += len(s)
				}
				n += 4
			default:
				n += 4
			}
		}
	}
	return n
}

func msgText(m *message.Msg) string {
	if t := m.GetTextContent(""); t != nil {
		return *t
	}
	return ""
}

func importedMsg(seq uint64, text string) *message.Msg {
	m := message.UserMsg("cloud", text)
	m.Metadata[ThreadSeqMetadataKey] = seq
	return m
}

func drain(t *testing.T, ch <-chan event.Event) []event.Event {
	t.Helper()
	var out []event.Event
	timeout := time.After(10 * time.Second)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, e)
		case <-timeout:
			t.Fatal("reply stream did not close")
			return out
		}
	}
}

func replyEnd(events []event.Event) *event.ReplyEndEvent {
	for i := len(events) - 1; i >= 0; i-- {
		if e, ok := events[i].(event.ReplyEndEvent); ok {
			return &e
		}
	}
	return nil
}

type errSaver struct {
	*fakeStateSaver
	err error
}

func (s *errSaver) SaveState(ctx context.Context, id string, st *AgentState) error {
	if s.err != nil {
		return s.err
	}
	return s.fakeStateSaver.SaveState(ctx, id, st)
}

func TestThreadSeqOfVariants(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    any
		want uint64
		ok   bool
	}{
		{"uint64", uint64(9), 9, true},
		{"uint", uint(9), 9, true},
		{"uint32", uint32(9), 9, true},
		{"int", 9, 9, true},
		{"int64", int64(9), 9, true},
		{"int32", int32(9), 9, true},
		{"float64", float64(9), 9, true},
		{"json.Number", json.Number("9"), 9, true},
		{"string", "9", 9, true},
		{"negative int", -1, 0, false},
		{"negative int64", int64(-1), 0, false},
		{"negative int32", int32(-1), 0, false},
		{"fraction", 9.5, 0, false},
		{"float overflow", 1e30, 0, false},
		{"not a number", "nine", 0, false},
		{"bool", true, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := message.UserMsg("u", "x")
			m.Metadata[ThreadSeqMetadataKey] = tc.v
			got, ok := ThreadSeqOf(m)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("ThreadSeqOf(%v) = %d, %v; want %d, %v", tc.v, got, ok, tc.want, tc.ok)
			}
		})
	}
	if _, ok := ThreadSeqOf(message.UserMsg("u", "x")); ok {
		t.Fatal("a message without the key has no seq")
	}
	if _, ok := ThreadSeqOf(nil); ok {
		t.Fatal("nil message has no seq")
	}
	// A JSON round trip turns uint64 into float64; the seq survives.
	m := importedMsg(1<<40, "x")
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back message.Msg
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if got, ok := ThreadSeqOf(&back); !ok || got != 1<<40 {
		t.Fatalf("round trip seq = %d, %v", got, ok)
	}
}

func TestThreadMessagesInjectedInOrder(t *testing.T) {
	a := NewUnifiedAgent("edge", "sys", &seamModel{})
	plain := a.prepareModelInput(context.Background())
	if len(plain) != 1 {
		t.Fatalf("an agent without thread state must send only the system prompt, got %d messages", len(plain))
	}

	a.state.Summary = "own summary"
	a.state.PeerSummary = &PeerSummary{Text: "peer text", Origin: "cloud/master", Through: 12}
	a.state.Plan = []tool.PlanItem{{Step: "step", Status: tool.PlanStatusInProgress}}
	a.state.Context = []*message.Msg{message.UserMsg("u", "hello")}
	got := a.prepareModelInput(context.Background())
	want := []string{
		"sys",
		"[Previous context summary]: own summary",
		"[Summary from cloud/master, covering the shared thread through #12]:\npeer text",
		"[Current plan]:\n1. [in_progress] step",
		"hello",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d", len(got), len(want))
	}
	for i := range want {
		if msgText(got[i]) != want[i] {
			t.Fatalf("message %d = %q, want %q", i, msgText(got[i]), want[i])
		}
	}
	for _, m := range got[1:4] {
		if m.Role != message.RoleUser {
			t.Fatalf("injected messages are user messages, got %s", m.Role)
		}
	}

	// An empty peer text injects no summary message.
	a.state.PeerSummary = &PeerSummary{Origin: "cloud/master", Through: 3}
	if n := len(a.prepareModelInput(context.Background())); n != len(want)-1 {
		t.Fatalf("empty peer text: got %d messages, want %d", n, len(want)-1)
	}
}

func TestCompressionAccountingCountsThreadMessages(t *testing.T) {
	base := compressionBaseMsgs("a", "sys", "own", threadPromptMsgs("a", &PeerSummary{Text: "peer", Origin: "o", Through: 1}), []tool.PlanItem{{Step: "s", Status: tool.PlanStatusPending}})
	var texts []string
	for _, m := range base {
		texts = append(texts, msgText(m))
	}
	if len(texts) != 4 || !strings.HasPrefix(texts[2], "[Summary from o") || !strings.HasPrefix(texts[3], "[Current plan]") {
		t.Fatalf("base messages out of order: %q", texts)
	}

	split := func(peer *PeerSummary) int {
		a := NewUnifiedAgent("a", "sys", &seamModel{tokens: charTokens})
		for i := 0; i < 10; i++ {
			a.state.Context = append(a.state.Context, message.UserMsg("u", strings.Repeat("x", 100)))
		}
		a.state.PeerSummary = peer
		_, reserved := a.splitContextForCompression(450, nil)
		return len(reserved)
	}
	without := split(nil)
	with := split(&PeerSummary{Text: strings.Repeat("p", 300), Origin: "cloud", Through: 1})
	if with >= without {
		t.Fatalf("the peer summary must be charged against the reserve: reserved %d with it, %d without", with, without)
	}
}

func TestCheckpointSchemaVersionFollowsThreadState(t *testing.T) {
	ctx := context.Background()
	saver := newFakeStateSaver()
	a := NewUnifiedAgent("a", "sys", &seamModel{}, WithStateSaver(saver))
	a.state.SessionID = "s"
	if err := a.SaveCheckpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if v := saver.states["s"].SchemaVersion; v != 1 {
		t.Fatalf("plain checkpoint schema = %d, want 1 (readable by older builds)", v)
	}
	for _, set := range []func(*AgentState){
		func(s *AgentState) { s.ThreadCursor = 4 },
		func(s *AgentState) { s.PeerSummary = &PeerSummary{Text: "p"} },
	} {
		b := NewUnifiedAgent("a", "sys", &seamModel{}, WithStateSaver(saver))
		b.state.SessionID = "t"
		set(b.state)
		if err := b.SaveCheckpoint(ctx); err != nil {
			t.Fatal(err)
		}
		if v := saver.states["t"].SchemaVersion; v != 3 {
			t.Fatalf("thread checkpoint schema = %d, want 3", v)
		}
		if err := b.ForkSession(ctx, "fork"); err != nil {
			t.Fatal(err)
		}
		if v := saver.states["fork"].SchemaVersion; v != 3 {
			t.Fatalf("forked thread session schema = %d, want 3", v)
		}
	}
	if err := a.ForkSession(ctx, "plain-fork"); err != nil {
		t.Fatal(err)
	}
	if v := saver.states["plain-fork"].SchemaVersion; v != 1 {
		t.Fatalf("plain fork schema = %d, want 1", v)
	}
}

func TestAgentStateV3RoundTrip(t *testing.T) {
	ctx := context.Background()
	st := &AgentState{
		SchemaVersion: 3,
		SessionID:     "s",
		Context:       []*message.Msg{importedMsg(3, "entry")},
		PeerSummary:   &PeerSummary{Text: "peer", Origin: "cloud/master", Through: 2, Excluded: []uint64{1}},
		ThreadCursor:  3,
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back AgentState
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.PeerSummary, st.PeerSummary) || back.ThreadCursor != 3 || back.SchemaVersion != 3 {
		t.Fatalf("thread state lost in round trip: %+v", back)
	}
	if seq, ok := ThreadSeqOf(back.Context[0]); !ok || seq != 3 {
		t.Fatalf("message seq lost: %d %v", seq, ok)
	}

	saver := newFakeStateSaver()
	saver.states["v3"] = &back
	loaded, err := LoadCheckpoint(ctx, saver, "v3")
	if err != nil || loaded.ThreadCursor != 3 {
		t.Fatalf("v3 checkpoint must load: %v %+v", err, loaded)
	}
	saver.states["v4"] = &AgentState{SchemaVersion: 4, SessionID: "v4"}
	if _, err := LoadCheckpoint(ctx, saver, "v4"); err == nil {
		t.Fatal("schema 4 must be rejected")
	}

	// A restored v3 state keeps working and re-saves as v3.
	a := NewUnifiedAgent("a", "sys", &seamModel{}, WithState(loaded), WithStateSaver(saver))
	if err := a.SaveCheckpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if saver.states["s"].SchemaVersion != 3 || saver.states["s"].ThreadCursor != 3 {
		t.Fatalf("restored state re-saved as %+v", saver.states["s"])
	}
}

func TestApplyThreadUpdateValidation(t *testing.T) {
	ctx := context.Background()
	a := NewUnifiedAgent("a", "sys", &seamModel{})
	a.state.ThreadCursor = 5
	a.state.Context = []*message.Msg{message.UserMsg("u", "keep")}
	before := append([]*message.Msg(nil), a.state.Context...)
	call := message.AssistantMsg("x", []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "c", Name: "t", Input: `{}`}})
	result := message.AssistantMsg("x", []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: "c", Name: "t", Output: "r"}})
	thinking := message.AssistantMsg("x", []message.ContentBlock{message.ThinkingBlock{Type: "thinking", Thinking: "hmm"}})
	for name, u := range map[string]ThreadUpdate{
		"system role":           {Messages: []*message.Msg{message.SystemMsg("s", "x")}},
		"tool call":             {Messages: []*message.Msg{call}},
		"tool result":           {Messages: []*message.Msg{result}},
		"thinking":              {Messages: []*message.Msg{thinking}},
		"cursor backwards":      {Cursor: 4},
		"set and clear summary": {PeerSummary: &PeerSummary{Text: "p"}, ClearPeerSummary: true},
	} {
		if err := a.ApplyThreadUpdate(ctx, u); err == nil {
			t.Fatalf("%s: update must be rejected", name)
		}
	}
	if a.state.ThreadCursor != 5 || !reflect.DeepEqual(a.state.Context, before) {
		t.Fatal("a rejected update changed state")
	}
	// Zero keeps the cursor; an equal cursor is allowed.
	if err := a.ApplyThreadUpdate(ctx, ThreadUpdate{Cursor: 5}); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyThreadUpdate(ctx, ThreadUpdate{Messages: []*message.Msg{importedMsg(6, "six")}}); err != nil {
		t.Fatal(err)
	}
	if a.state.ThreadCursor != 5 || len(a.state.Context) != 2 {
		t.Fatalf("unexpected state after valid updates: cursor=%d len=%d", a.state.ThreadCursor, len(a.state.Context))
	}
}

func TestApplyThreadUpdateRemovesCoveredMessages(t *testing.T) {
	a := NewUnifiedAgent("edge", "sys", &seamModel{})
	own := message.AssistantMsg("edge", "own turn")
	ownUser := message.UserMsg("user", "user turn")
	a.state.Context = []*message.Msg{importedMsg(1, "one"), ownUser, importedMsg(2, "two"), importedMsg(3, "three"), own, importedMsg(4, "four"), importedMsg(5, "five")}
	a.lastModelInputTokens = 900
	ps := &PeerSummary{Text: "covers 1-4 but 3", Origin: "cloud/master", Through: 4, Excluded: []uint64{3}}
	if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{PeerSummary: ps, Messages: []*message.Msg{importedMsg(6, "six")}, Cursor: 6}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range a.state.Context {
		got = append(got, msgText(m))
	}
	want := []string{"user turn", "three", "own turn", "five", "six"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("context after removal = %q, want %q", got, want)
	}
	if a.state.Context[0] != ownUser || a.state.Context[2] != own {
		t.Fatal("kept messages must stay the same live messages")
	}
	if a.state.ThreadCursor != 6 || a.state.PeerSummary.Through != 4 || a.lastModelInputTokens != 0 {
		t.Fatalf("cursor=%d summary=%+v floor=%d", a.state.ThreadCursor, a.state.PeerSummary, a.lastModelInputTokens)
	}
	// The slot holds a copy: later caller edits do not leak in.
	ps.Excluded[0] = 99
	if a.state.PeerSummary.Excluded[0] != 3 {
		t.Fatal("the peer summary slot must not alias the caller's value")
	}
	if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{ClearPeerSummary: true}); err != nil {
		t.Fatal(err)
	}
	if a.state.PeerSummary != nil {
		t.Fatal("ClearPeerSummary must clear the slot")
	}
}

func TestApplyThreadUpdateRejectsEmptyPeerSummary(t *testing.T) {
	a := NewUnifiedAgent("edge", "sys", &seamModel{})
	covered := importedMsg(2, "covered entry")
	a.state.Context = []*message.Msg{covered}
	for name, ps := range map[string]*PeerSummary{
		"empty": {Origin: "cloud/master", Through: 5},
		"blank": {Text: " \n\t", Origin: "cloud/master", Through: 5},
	} {
		err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{PeerSummary: ps})
		if err == nil || !strings.Contains(err.Error(), "no text") {
			t.Fatalf("%s summary: err = %v", name, err)
		}
	}
	if a.state.PeerSummary != nil || len(a.state.Context) != 1 || a.state.Context[0] != covered {
		t.Fatal("a rejected summary must not remove the messages it would cover")
	}
}

// TestPeerSummaryKeepsSeqTaggedToolMessages: removal never takes a message
// holding tool blocks, even when its seq is covered, so no tool_call loses its
// tool_result.
func TestPeerSummaryKeepsSeqTaggedToolMessages(t *testing.T) {
	a := NewUnifiedAgent("edge", "sys", &seamModel{})
	turn := message.AssistantMsg("edge", []message.ContentBlock{
		message.ToolCallBlock{Type: "tool_call", ID: "c", Name: "echo", Input: `{}`, State: message.ToolCallFinished},
		message.ToolResultBlock{Type: "tool_result", ID: "c", Name: "echo", Output: "r", State: message.ToolResultSuccess},
	})
	turn.Metadata[ThreadSeqMetadataKey] = uint64(2)
	a.state.Context = []*message.Msg{importedMsg(1, "one"), turn, importedMsg(3, "three")}
	if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{PeerSummary: &PeerSummary{Text: "covers 1-3", Through: 3}}); err != nil {
		t.Fatal(err)
	}
	if len(a.state.Context) != 1 || a.state.Context[0] != turn {
		t.Fatalf("only the tool turn may stay, got %d messages", len(a.state.Context))
	}
}

// stubbornTool ignores cancellation: it returns only once released.
type stubbornTool struct {
	tool.BaseTool
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newStubbornTool(name string) *stubbornTool {
	t := &stubbornTool{started: make(chan struct{}), release: make(chan struct{})}
	t.ToolName = name
	t.ToolDescription = "ignores cancellation"
	t.ToolSchema = json.RawMessage(`{"type":"object","properties":{}}`)
	return t
}

func (t *stubbornTool) Execute(context.Context, map[string]any) (*tool.ToolResponse, error) {
	t.once.Do(func() { close(t.started) })
	<-t.release
	return tool.NewTextResponse("finished late"), nil
}

// cancelStopMiddleware forwards reply events until ctx ends and then stops,
// closing its stream even while the reply core keeps running.
type cancelStopMiddleware struct{ middleware.BaseMiddleware }

func (*cancelStopMiddleware) OnReply(ctx context.Context, input middleware.ReplyInput, next middleware.ReplyHandler) <-chan event.Event {
	in := next(ctx, input)
	out := make(chan event.Event, 16)
	go func() {
		defer close(out)
		for {
			select {
			case evt, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- evt:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// TestBetweenRepliesSeamsWaitForTheReplyCore: a canceled reply's stream can
// close while its core is still inside a tool that ignores cancellation. The
// between-replies seams must refuse until the core has returned; a reset
// applied under the running core would leave its late tool result orphaned.
func TestBetweenRepliesSeamsWaitForTheReplyCore(t *testing.T) {
	slow := newStubbornTool("slow")
	m := &seamModel{tokens: func([]*message.Msg) int { return 1 }}
	m.respond = func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		if call == 0 {
			return seamToolCalls(message.ToolCallBlock{ID: "s1", Name: "slow"}), nil
		}
		return strictSummaryResponse(), nil
	}
	a := NewUnifiedAgent("a", "sys", m, WithMiddlewares(&cancelStopMiddleware{}), WithToolkit(tool.NewToolkit(slow)),
		WithContextConfig(&ContextConfig{ContextSize: 1000}))
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := a.ReplyStream(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-slow.started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not start")
	}
	cancel()
	drain(t, ch)

	a.mu.Lock()
	replies, idle := a.activeReplies, a.coresIdle
	a.mu.Unlock()
	if replies != 0 || idle == nil {
		t.Fatalf("setup: want a closed stream over a running core, got activeReplies=%d core running=%v", replies, idle != nil)
	}
	bg := context.Background()
	busy := func(name string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "idle agent") || !errors.Is(err, ErrReplyInProgress) {
			t.Fatalf("%s while the reply core runs: err = %v", name, err)
		}
	}
	busy("ApplyThreadUpdate", a.ApplyThreadUpdate(bg, ThreadUpdate{Reset: true}))
	busy("ApplySummary", a.ApplySummary(bg, &ForkSnapshot{}, "summary", SummaryApplyOptions{}))
	_, err = a.CompactStrict(bg, CompactStrictOptions{})
	busy("CompactStrict", err)

	close(slow.release)
	select {
	case <-idle:
	case <-time.After(5 * time.Second):
		t.Fatal("the reply core did not return")
	}
	// The late result sits next to its call: nothing reset the context under
	// the running core.
	calls, results := map[string]bool{}, 0
	for _, msg := range a.state.Context {
		for _, b := range msg.GetContentBlocks(message.ContentBlockToolCall) {
			calls[b.(message.ToolCallBlock).ID] = true
		}
		for _, b := range msg.GetContentBlocks(message.ContentBlockToolResult) {
			results++
			if id := b.(message.ToolResultBlock).ID; !calls[id] {
				t.Fatalf("tool result %q has no call before it", id)
			}
		}
	}
	if results != 1 || a.state.Summary != "" {
		t.Fatalf("results=%d summary=%q", results, a.state.Summary)
	}
	if err := a.ApplyThreadUpdate(bg, ThreadUpdate{Reset: true}); err != nil {
		t.Fatalf("once the core returned the agent is idle: %v", err)
	}
}

// TestReplyEndLeavesNoRunningCore: a consumer that saw the stream end must
// find the agent idle at once, never a core still being counted.
func TestReplyEndLeavesNoRunningCore(t *testing.T) {
	m := &seamModel{respond: func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) { return seamText("ok"), nil }}
	for _, mws := range [][]middleware.Middleware{nil, {&testLoggingMiddleware{}}} {
		a := NewUnifiedAgent("a", "sys", m, WithMiddlewares(mws...))
		for i := 0; i < 50; i++ {
			if _, err := a.Reply(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}
			if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{Cursor: uint64(i + 1)}); err != nil {
				t.Fatalf("reply %d: %v", i, err)
			}
		}
	}
}

func TestApplyThreadUpdateResetScope(t *testing.T) {
	cfg := &ContextConfig{ContextSize: 1000}
	tk := tool.NewToolkit(tool.NewFunctionTool("probe", "p", nil, func(context.Context, map[string]any) (any, error) { return "ok", nil }))
	a := NewUnifiedAgent("a", "sys", &seamModel{}, WithContextConfig(cfg), WithToolkit(tk),
		WithPermissionContext(permission.NewContext(permission.ModeDefault)))
	a.engine.AddWorkingDirectory(permission.WorkingDirectory{Path: "/granted", Source: "granted"})
	a.state.Context = []*message.Msg{message.UserMsg("u", "old")}
	a.state.Summary = "old summary"
	a.state.Plan = []tool.PlanItem{{Step: "keep", Status: tool.PlanStatusPending}}
	a.state.ReadCacheData = json.RawMessage(`{}`)
	a.readCache.CacheFile("/tmp/x", []string{"line"})
	a.lastModelInputTokens = 500
	a.pendingPrecompress = &precompressResult{summary: "stale"}
	before := a.compactionVersion
	if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{Reset: true, Messages: []*message.Msg{importedMsg(9, "fresh")}, Cursor: 9}); err != nil {
		t.Fatal(err)
	}
	if len(a.state.Context) != 1 || msgText(a.state.Context[0]) != "fresh" {
		t.Fatalf("reset must clear the context before the new messages apply: %d", len(a.state.Context))
	}
	if a.state.Summary != "" || a.readCache.Len() != 0 || a.state.ReadCacheData != nil || a.pendingPrecompress != nil || a.lastModelInputTokens != 0 || a.compactionVersion == before {
		t.Fatal("reset must clear summary, read cache, pending precompression and the token floor")
	}
	if len(a.state.Plan) != 1 || a.toolkit.Get("probe") == nil || len(a.engine.GrantedDirectories()) != 1 {
		t.Fatal("reset must keep the plan, the toolkit and permission grants")
	}
	if a.state.ThreadCursor != 9 {
		t.Fatal("the rest of the update applies after the reset")
	}
}

func TestApplyThreadUpdatePersists(t *testing.T) {
	ctx := context.Background()
	saver := &errSaver{fakeStateSaver: newFakeStateSaver()}
	a := NewUnifiedAgent("a", "sys", &seamModel{}, WithStateSaver(saver))
	a.state.SessionID = "s"
	if err := a.ApplyThreadUpdate(ctx, ThreadUpdate{Messages: []*message.Msg{importedMsg(3, "entry")}, Cursor: 3}); err != nil {
		t.Fatal(err)
	}
	saved := saver.states["s"]
	if saved == nil || saved.SchemaVersion != 3 || saved.ThreadCursor != 3 || len(saved.Context) != 1 {
		t.Fatalf("cursor and content must land in one checkpoint: %+v", saved)
	}

	saver.err = errors.New("disk full")
	if err := a.ApplyThreadUpdate(ctx, ThreadUpdate{Cursor: 4}); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("persistence failure must be returned, got %v", err)
	}
	if a.state.ThreadCursor != 4 {
		t.Fatal("a persistence failure leaves the update applied in memory")
	}
	saver.err = nil
	if err := a.ApplyThreadUpdate(ctx, ThreadUpdate{Cursor: 5}); err != nil || saver.states["s"].ThreadCursor != 5 {
		t.Fatalf("after the failure clears: err=%v", err)
	}
}

func TestApplyThreadUpdateRejectedWhileBusy(t *testing.T) {
	gate := newGatedTool("gate")
	m := &seamModel{respond: func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		if call == 0 {
			return seamToolCalls(message.ToolCallBlock{ID: "g", Name: "gate"}), nil
		}
		return seamText("done"), nil
	}}
	a := NewUnifiedAgent("a", "sys", m, WithToolkit(tool.NewToolkit(gate)))
	ch, err := a.ReplyStream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	<-gate.started
	if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{Cursor: 1}); err == nil {
		t.Fatal("ApplyThreadUpdate must be rejected during a reply")
	}
	close(gate.gate)
	drain(t, ch)
	if a.state.ThreadCursor != 0 {
		t.Fatal("a rejected update changed state")
	}

	conv, err := a.NewConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{Cursor: 1}); err == nil {
		t.Fatal("ApplyThreadUpdate must be rejected while a conversation owns the agent")
	}
	_ = conv.Close()
	if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{Cursor: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestBoundaryInputLandsAfterToolResult(t *testing.T) {
	echo := tool.NewFunctionTool("echo", "e", nil, func(context.Context, map[string]any) (any, error) { return "echoed", nil })
	m := &seamModel{respond: func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		if call == 0 {
			return seamToolCalls(message.ToolCallBlock{ID: "c1", Name: "echo"}), nil
		}
		return seamText("done"), nil
	}}
	var boundaries int
	src := func(context.Context) (*ThreadUpdate, error) {
		boundaries++
		if boundaries != 2 { // the boundary after the tool step
			return nil, nil
		}
		return &ThreadUpdate{Messages: []*message.Msg{importedMsg(7, "edge note")}, Cursor: 7}, nil
	}
	saver := newFakeStateSaver()
	a := NewUnifiedAgent("a", "sys", m, WithToolkit(tool.NewToolkit(echo)), WithBoundaryInput(src), WithStateSaver(saver))
	a.state.SessionID = "s"
	ch, err := a.ReplyStream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	events := drain(t, ch)
	if end := replyEnd(events); end == nil || end.FinishedReason != types.ReplyCompleted {
		t.Fatalf("reply end = %+v", end)
	}
	var injected *event.CustomEvent
	for _, e := range events {
		if c, ok := e.(event.CustomEvent); ok && c.Name == ThreadEventInputInjected {
			injected = &c
		}
	}
	if injected == nil || injected.Value["cursor"] != uint64(7) || injected.Value["count"] != 1 {
		t.Fatalf("missing or wrong %s event: %+v", ThreadEventInputInjected, injected)
	}
	if m.callCount() != 2 {
		t.Fatalf("model calls = %d", m.callCount())
	}
	second := m.call(1).msgs
	last, prev := second[len(second)-1], second[len(second)-2]
	if msgText(last) != "edge note" || last.Role != message.RoleUser {
		t.Fatalf("the imported entry must be the last input before the next call, got %q", msgText(last))
	}
	if len(prev.GetContentBlocks(message.ContentBlockToolCall)) != 1 || len(prev.GetContentBlocks(message.ContentBlockToolResult)) != 1 {
		t.Fatal("the tool call and its result must stay together before the imported entry")
	}
	if v := a.ThreadView(); v.Cursor != 7 {
		t.Fatalf("cursor = %d", v.Cursor)
	}
	saved := saver.states["s"]
	if saved == nil || saved.SchemaVersion != 3 || saved.ThreadCursor != 7 {
		t.Fatalf("boundary input must be persisted: %+v", saved)
	}
	// Final context: user, assistant(call, result), imported, assistant(done).
	ctxMsgs := a.state.Context
	if len(ctxMsgs) != 4 || msgText(ctxMsgs[2]) != "edge note" || msgText(ctxMsgs[3]) != "done" {
		t.Fatalf("unexpected final context of %d messages", len(ctxMsgs))
	}
}

func TestBoundaryInputFailuresEndTheReply(t *testing.T) {
	for name, src := range map[string]BoundaryInput{
		"source error": func(context.Context) (*ThreadUpdate, error) { return nil, errors.New("log unavailable") },
		"reset":        func(context.Context) (*ThreadUpdate, error) { return &ThreadUpdate{Reset: true}, nil },
		"invalid": func(context.Context) (*ThreadUpdate, error) {
			return &ThreadUpdate{Messages: []*message.Msg{message.SystemMsg("s", "x")}}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := &seamModel{respond: func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) { return seamText("x"), nil }}
			a := NewUnifiedAgent("a", "sys", m, WithBoundaryInput(src))
			_, err := a.Reply(context.Background(), "go")
			if err == nil || !strings.Contains(err.Error(), "thread input failed") {
				t.Fatalf("reply error = %v", err)
			}
			if m.callCount() != 0 {
				t.Fatal("no model call may follow a failed boundary input")
			}
		})
	}
	t.Run("persistence", func(t *testing.T) {
		m := &seamModel{respond: func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) { return seamText("x"), nil }}
		saver := &errSaver{fakeStateSaver: newFakeStateSaver(), err: errors.New("disk full")}
		src := func(context.Context) (*ThreadUpdate, error) { return &ThreadUpdate{Cursor: 2}, nil }
		a := NewUnifiedAgent("a", "sys", m, WithBoundaryInput(src), WithStateSaver(saver))
		if _, err := a.Reply(context.Background(), "go"); err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("reply error = %v", err)
		}
	})
}

// TestBoundaryInputCanceledEndsInterrupted: a source that fails because the
// reply was canceled after the loop's own cancellation check ends the reply
// as interrupted, not as a thread input error. A Conversation turn emits its
// end on the conversation context, so the end survives the cancellation.
func TestBoundaryInputCanceledEndsInterrupted(t *testing.T) {
	var c *Conversation
	src := func(ctx context.Context) (*ThreadUpdate, error) {
		c.Interrupt()
		<-ctx.Done()
		return nil, errors.New("log read aborted")
	}
	m := &seamModel{respond: func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) { return seamText("x"), nil }}
	a := NewUnifiedAgent("a", "sys", m, WithBoundaryInput(src))
	var err error
	if c, err = a.NewConversation(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Submit("go"); err != nil {
		t.Fatal(err)
	}
	evs := convWait(t, c, "reply end", func(e event.Event) bool { _, ok := isReplyEnd(e); return ok })
	end, _ := isReplyEnd(evs[len(evs)-1])
	if end.FinishedReason != types.ReplyInterrupted || end.Error != nil {
		t.Fatalf("end = %s (%+v), want interrupted", end.FinishedReason, end.Error)
	}
	if m.callCount() != 0 {
		t.Fatal("no model call may follow a canceled boundary input")
	}
}

func TestThreadInputTagsReplyMessage(t *testing.T) {
	m := &seamModel{respond: func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) { return seamText("ok"), nil }}
	a := NewUnifiedAgent("a", "sys", m)
	userMsg := func() *message.Msg {
		for i := len(a.state.Context) - 1; i >= 0; i-- {
			if a.state.Context[i].Role == message.RoleUser {
				return a.state.Context[i]
			}
		}
		return nil
	}
	if _, err := a.Reply(WithThreadInput(context.Background(), 5), "five"); err != nil {
		t.Fatal(err)
	}
	if seq, ok := ThreadSeqOf(userMsg()); !ok || seq != 5 || a.state.ThreadCursor != 5 {
		t.Fatalf("seq=%d ok=%v cursor=%d", seq, ok, a.state.ThreadCursor)
	}
	if _, err := a.Reply(WithThreadInput(context.Background(), 3), "three"); err != nil {
		t.Fatal(err)
	}
	if seq, _ := ThreadSeqOf(userMsg()); seq != 3 || a.state.ThreadCursor != 5 {
		t.Fatalf("a lower seq tags the message but never lowers the cursor: seq=%d cursor=%d", seq, a.state.ThreadCursor)
	}
	if _, err := a.Reply(context.Background(), "plain"); err != nil {
		t.Fatal(err)
	}
	if _, ok := userMsg().Metadata[ThreadSeqMetadataKey]; ok {
		t.Fatal("a reply without WithThreadInput carries no seq")
	}
}

func TestThreadViewTokensAndWindow(t *testing.T) {
	perMsg := func(msgs []*message.Msg) int { return 100 * len(msgs) }
	m := &seamModel{tokens: perMsg, window: 1000}
	a := NewUnifiedAgent("a", "sys", m)
	a.state.PeerSummary = &PeerSummary{Text: "peer", Through: 4}
	a.state.ThreadCursor = 5
	a.state.Context = []*message.Msg{message.UserMsg("u", "a"), message.UserMsg("u", "b")}
	v := a.ThreadView()
	// system + peer summary + two context messages.
	if v.InputTokens != 400 || v.Window != 1000 || v.ContextMessages != 2 || v.Cursor != 5 {
		t.Fatalf("view = %+v", v)
	}
	v.PeerSummary.Text = "changed"
	if a.state.PeerSummary.Text != "peer" {
		t.Fatal("the view must hold a copy of the peer summary")
	}
	a.lastModelInputTokens = 900
	if v := a.ThreadView(); v.InputTokens != 900 {
		t.Fatalf("the provider-grounded floor applies: %d", v.InputTokens)
	}

	cfg := NewUnifiedAgent("a", "sys", m, WithContextConfig(&ContextConfig{ContextSize: 5000}))
	if w := cfg.ThreadView().Window; w != 5000 {
		t.Fatalf("configured window = %d", w)
	}
	fb := NewUnifiedAgent("a", "sys", m, WithModelConfig(ModelConfig{FallbackModel: &seamModel{window: 800}}))
	if w := fb.ThreadView().Window; w != 800 {
		t.Fatalf("fallback window = %d", w)
	}
	unknown := NewUnifiedAgent("a", "sys", &seamModel{})
	if w := unknown.ThreadView().Window; w != 0 {
		t.Fatalf("unknown window = %d, want 0", w)
	}
}

// promptCountingMiddleware counts OnSystemPrompt runs and grows the prompt.
type promptCountingMiddleware struct {
	middleware.BaseMiddleware
	runs atomic.Int32
}

func (m *promptCountingMiddleware) OnSystemPrompt(_ context.Context, _ string, p string) string {
	m.runs.Add(1)
	return p + strings.Repeat(" memory", 100)
}

// TestThreadViewRunsNoMiddleware: ThreadView reads state under the agent lock
// and must not run OnSystemPrompt middleware there (it may do file I/O). Its
// estimate counts the raw system prompt.
func TestThreadViewRunsNoMiddleware(t *testing.T) {
	m := &seamModel{tokens: charTokens}
	mw := &promptCountingMiddleware{}
	a := NewUnifiedAgent("a", "sys", m, WithMiddlewares(mw))
	a.state.PeerSummary = &PeerSummary{Text: "peer", Through: 1}
	a.state.Context = []*message.Msg{message.UserMsg("u", "hello")}
	v := a.ThreadView()
	if n := mw.runs.Load(); n != 0 {
		t.Fatalf("ThreadView ran OnSystemPrompt %d times", n)
	}
	raw := NewUnifiedAgent("a", "sys", m)
	raw.state.PeerSummary = &PeerSummary{Text: "peer", Through: 1}
	raw.state.Context = []*message.Msg{message.UserMsg("u", "hello")}
	if want := raw.ThreadView().InputTokens; v.InputTokens != want || want == 0 {
		t.Fatalf("estimate = %d, want the raw-prompt estimate %d", v.InputTokens, want)
	}
	// The model input itself still goes through the pipeline.
	if got := msgText(a.prepareModelInput(context.Background())[0]); !strings.HasPrefix(got, "sys memory") || mw.runs.Load() != 1 {
		t.Fatalf("prepareModelInput prompt = %.20q (runs %d)", got, mw.runs.Load())
	}
}

// TestStructuredResponseIncludesThreadMessages: report generation sees the
// same thread messages as a reply's model call, in the same place.
func TestStructuredResponseIncludesThreadMessages(t *testing.T) {
	state := &AgentState{
		SessionID:   "s",
		Summary:     "own",
		PeerSummary: &PeerSummary{Text: "peer text", Origin: "cloud/master", Through: 3},
		Context:     []*message.Msg{message.UserMsg("u", "hello")},
	}
	var got []string
	m := &structuredUsageModel{call: func(_ context.Context, msgs []*message.Msg, _ model.CallOptions) (*model.ChatResponse, error) {
		got = got[:0]
		for _, msg := range msgs {
			got = append(got, msgText(msg))
		}
		return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", Name: "generate_structured_output", Input: `{"report":"done"}`}},
			Usage: &model.ChatUsage{InputTokens: 3, OutputTokens: 4}}, nil
	}}
	a := NewUnifiedAgent("a", "system", m, WithState(state))
	if _, err := a.GenerateStructuredResponseWithUsage(context.Background(), json.RawMessage(`{"type":"object"}`)); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"system",
		"[Previous context summary]: own",
		"[Summary from cloud/master, covering the shared thread through #3]:\npeer text",
		"hello",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("structured request = %q, want %q", got, want)
	}
}

// TestPrecompressChargesThreadMessages: the background split charges the
// peer summary against the reserve, like the synchronous one.
func TestPrecompressChargesThreadMessages(t *testing.T) {
	reserved := func(peer *PeerSummary) int {
		m := &seamModel{tokens: charTokens, respond: func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
			return strictSummaryResponse(), nil
		}}
		a := NewUnifiedAgent("a", "sys", m)
		for i := 0; i < 10; i++ {
			a.state.Context = append(a.state.Context, message.UserMsg("u", strings.Repeat("x", 100)))
		}
		a.state.PeerSummary = peer
		// A 2000-token window with a 0.225 reserve ratio reserves 450 tokens.
		cfg := (&ContextConfig{ContextSize: 2000, TriggerRatio: 0.8, ReserveRatio: 0.225, PrecompressRatio: 0.5, ThreadSummaryTokens: -1}).withDefaults()
		a.maybeArmPrecompress(context.Background(), &cfg, 2000, 1500)
		p := waitPrecompress(t, a)
		if p == nil {
			t.Fatal("expected a background result")
		}
		return p.reservedLen
	}
	without := reserved(nil)
	with := reserved(&PeerSummary{Text: strings.Repeat("p", 300), Origin: "cloud", Through: 1})
	if with >= without {
		t.Fatalf("the peer summary must be charged against the reserve: reserved %d with it, %d without", with, without)
	}
}

// TestResetKeepsTheCursorRule: a clear is one update, so its cursor is never
// behind the current one; a Reset may not move the cursor back either.
func TestResetKeepsTheCursorRule(t *testing.T) {
	a := NewUnifiedAgent("a", "sys", &seamModel{})
	a.state.ThreadCursor = 6
	a.state.Context = []*message.Msg{importedMsg(5, "five")}
	ctx := context.Background()
	if err := a.ApplyThreadUpdate(ctx, ThreadUpdate{Reset: true, Messages: []*message.Msg{importedMsg(4, "four")}, Cursor: 4}); err == nil {
		t.Fatal("a reset must not move the cursor back")
	}
	if a.state.ThreadCursor != 6 || len(a.state.Context) != 1 || msgText(a.state.Context[0]) != "five" {
		t.Fatalf("a rejected reset changed the state: cursor=%d context=%d", a.state.ThreadCursor, len(a.state.Context))
	}
	if err := a.ApplyThreadUpdate(ctx, ThreadUpdate{Reset: true, Messages: []*message.Msg{importedMsg(7, "seven")}, Cursor: 7}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if a.state.ThreadCursor != 7 || len(a.state.Context) != 1 || msgText(a.state.Context[0]) != "seven" {
		t.Fatalf("cursor=%d context=%d", a.state.ThreadCursor, len(a.state.Context))
	}
}

// TestThreadSummaryTokensSharesTheBudget: with ThreadSummaryTokens set, the
// own summary gets what a held peer summary leaves (at least 256), a peer
// summary that arrives elides the own one to that share at once, and the
// thread messages are given back to the reserve.
func TestThreadSummaryTokensSharesTheBudget(t *testing.T) {
	cfg := &ContextConfig{ContextSize: 8000, ThreadSummaryTokens: 1500}
	m := &seamModel{tokens: func(msgs []*message.Msg) int {
		n := 0
		for _, x := range msgs {
			n += len(msgText(x)) / 4
		}
		return n
	}}
	a := NewUnifiedAgent("a", "sys", m, WithContextConfig(cfg))
	a.state.Summary = strings.Repeat("o", 8000) // 2,000 estimated tokens
	ctx := context.Background()
	peer := &PeerSummary{Text: strings.Repeat("p", 2400), Through: 3} // 600
	if err := a.ApplyThreadUpdate(ctx, ThreadUpdate{PeerSummary: peer, Cursor: 3}); err != nil {
		t.Fatal(err)
	}
	if n := len(a.state.Summary) / 4; n > 920 || n < 850 { // the elision marker adds a few
		t.Fatalf("own summary is %d tokens after a 600-token peer summary; want its 900-token share", n)
	}
	c := cfg.withDefaults()
	if got := a.ownSummaryBudget(&c, 8000); got != 900 {
		t.Fatalf("own budget = %d, want 900", got)
	}
	big := &PeerSummary{Text: strings.Repeat("p", 6000), Through: 4} // 1,500
	if err := a.ApplyThreadUpdate(ctx, ThreadUpdate{PeerSummary: big, Cursor: 4}); err != nil {
		t.Fatal(err)
	}
	if got := a.ownSummaryBudget(&c, 8000); got != minOwnSummaryTokens {
		t.Fatalf("own budget next to a full peer summary = %d, want %d", got, minOwnSummaryTokens)
	}
	base := int(8000 * c.ReserveRatio)
	thread := m.CountTokens(threadPromptMsgs(a.name, big), nil)
	if got := a.reserveTokens(&c, 8000); got != base+thread {
		t.Fatalf("reserve = %d, want %d + %d", got, base, thread)
	}

	off := NewUnifiedAgent("b", "sys", m, WithContextConfig(&ContextConfig{ContextSize: 8000, ThreadSummaryTokens: -1}))
	off.state.Summary = strings.Repeat("o", 8000)
	if err := off.ApplyThreadUpdate(ctx, ThreadUpdate{PeerSummary: peer, Cursor: 3}); err != nil {
		t.Fatal(err)
	}
	oc := off.contextCfg.withDefaults()
	if len(off.state.Summary) != 8000 || off.reserveTokens(&oc, 8000) != int(8000*oc.ReserveRatio) {
		t.Fatal("with ThreadSummaryTokens off nothing changes")
	}
}

// TestThreadSummaryTokensAutoAndAtRunTime: zero sizes the budget at 3/8 of
// the window, only while a peer summary is held; SetThreadSummaryTokens
// changes it at run time and elides the own summary to the new share at once.
func TestThreadSummaryTokensAutoAndAtRunTime(t *testing.T) {
	m := &seamModel{tokens: func(msgs []*message.Msg) int {
		n := 0
		for _, x := range msgs {
			n += len(msgText(x)) / 4
		}
		return n
	}}
	a := NewUnifiedAgent("a", "sys", m, WithContextConfig(&ContextConfig{ContextSize: 8192}))
	c := a.contextCfg.withDefaults()
	if got, want := a.ownSummaryBudget(&c, 8192), summaryMaxTokens(&c, 8192); got != want {
		t.Fatalf("budget without a peer summary = %d, want the SummaryMaxTokens budget %d", got, want)
	}
	a.state.Summary = strings.Repeat("o", 12000)                      // 3,000 estimated tokens
	peer := &PeerSummary{Text: strings.Repeat("p", 8000), Through: 3} // 2,000
	if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{PeerSummary: peer, Cursor: 3}); err != nil {
		t.Fatal(err)
	}
	// 3/8 of 8,192 is 3,072; the peer summary leaves 1,072, above the
	// 1,024-token SummaryMaxTokens budget of this window.
	if got := a.ownSummaryBudget(&c, 8192); got != 1024 {
		t.Fatalf("automatic own budget = %d, want 1024", got)
	}
	if n := len(a.state.Summary) / 4; n > 1100 || n < 1000 {
		t.Fatalf("own summary is %d tokens next to the peer summary; want its 1,072-token share", n)
	}
	if got := a.ThreadView().SummaryTokens; got != 3072 {
		t.Fatalf("reported automatic budget = %d, want 3072", got)
	}
	a.SetThreadSummaryTokens(2500)
	if got := a.ThreadView().SummaryTokens; got != 2500 {
		t.Fatalf("reported budget after SetThreadSummaryTokens(2500) = %d", got)
	}
	if got := a.ownSummaryBudget(&c, 8192); got != 500 {
		t.Fatalf("own budget after SetThreadSummaryTokens(2500) = %d, want 500", got)
	}
	if n := len(a.state.Summary) / 4; n > 530 {
		t.Fatalf("own summary is %d tokens after the budget fell; want its 500-token share", n)
	}
	a.SetThreadSummaryTokens(-1)
	if got := a.ThreadView().SummaryTokens; got != 0 {
		t.Fatalf("reported budget with the option off = %d", got)
	}
	if got, want := a.ownSummaryBudget(&c, 8192), summaryMaxTokens(&c, 8192); got != want {
		t.Fatalf("budget with the option off = %d, want %d", got, want)
	}
}

// TestThreadViewCompactAt: the view reports where automatic compression
// fires, and 0 without a ContextConfig or a known window.
func TestThreadViewCompactAt(t *testing.T) {
	a := NewUnifiedAgent("a", "sys", &seamModel{tokens: charTokens}, WithContextConfig(&ContextConfig{ContextSize: 10000, TriggerRatio: 0.5}))
	if got := a.ThreadView().CompactAt; got != 5000 {
		t.Fatalf("CompactAt = %d, want 5000", got)
	}
	b := NewUnifiedAgent("b", "sys", &seamModel{tokens: charTokens}, WithContextConfig(&ContextConfig{ContextSize: 10000}))
	if got := b.ThreadView().CompactAt; got != 8000 {
		t.Fatalf("CompactAt with the default ratio = %d, want 8000", got)
	}
	if got := NewUnifiedAgent("c", "sys", &seamModel{tokens: charTokens}).ThreadView().CompactAt; got != 0 {
		t.Fatalf("CompactAt without a ContextConfig = %d, want 0", got)
	}
}

// TestCloneMsgsKeepsNils: the copies keep nil slices and nil entries, and
// own their content slices.
func TestCloneMsgsKeepsNils(t *testing.T) {
	if cloneMsgs(nil) != nil {
		t.Fatal("a nil slice stays nil")
	}
	m := message.UserMsg("u", "hello")
	out := cloneMsgs([]*message.Msg{nil, m})
	if len(out) != 2 || out[0] != nil || out[1] == m || msgText(out[1]) != "hello" {
		t.Fatalf("clone = %+v", out)
	}
	out[1].Content[0] = message.TextBlock{Type: "text", Text: "changed"}
	if msgText(m) != "hello" {
		t.Fatal("the copy must own its content slice")
	}
}

// TestApplyThreadUpdateNilContextAndNilMessages: a nil context means
// context.Background, and nil messages are skipped.
func TestApplyThreadUpdateNilContextAndNilMessages(t *testing.T) {
	a := NewUnifiedAgent("a", "sys", &seamModel{})
	var ctx context.Context // nil: the case under test
	if err := a.ApplyThreadUpdate(ctx, ThreadUpdate{Messages: []*message.Msg{nil, message.UserMsg("u", "entry")}, Cursor: 2}); err != nil {
		t.Fatal(err)
	}
	if v := a.ThreadView(); v.Cursor != 2 || v.ContextMessages != 1 {
		t.Fatalf("view = %+v", v)
	}
}

// TestOwnSummaryShareUsesTheSmallerFallbackWindow: without a configured
// ContextSize the automatic budget is sized from the smaller of the model's
// and the fallback model's windows.
func TestOwnSummaryShareUsesTheSmallerFallbackWindow(t *testing.T) {
	a := NewUnifiedAgent("a", "sys", &compressionMockModel{contextSize: 200000},
		WithModelConfig(ModelConfig{FallbackModel: &compressionMockModel{contextSize: 8192}}),
		WithContextConfig(&ContextConfig{}))
	a.state.Summary = strings.Repeat("o", 12000)                      // 3,000 estimated tokens
	peer := &PeerSummary{Text: strings.Repeat("p", 8000), Through: 3} // 2,000
	if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{PeerSummary: peer, Cursor: 3}); err != nil {
		t.Fatal(err)
	}
	// 3/8 of the fallback's 8,192 is 3,072; the peer summary leaves 1,072.
	// The primary's 200,000-token window would leave room for all of it.
	if n := len(a.state.Summary) / 4; n > 1100 || n < 1000 {
		t.Fatalf("own summary is %d tokens; want the 1,072-token share of the fallback's window", n)
	}
}
