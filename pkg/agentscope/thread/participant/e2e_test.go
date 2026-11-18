package participant_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/pipeline"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread/participant"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread/route"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// This file runs the Main Thread end to end: a real Coordinator, the router
// with the offline classifier, an edge AgentParticipant and a cloud
// TeamParticipant, each over real UnifiedAgents whose models are scripted and
// record every request they receive.

// doorCode is the code the user has the edge set on the front door lock.
const doorCode = "CODE-7f3a"

// ---------------------------------------------------------------- wire model

// wireMsg is one message of a recorded request.
type wireMsg struct {
	role message.Role
	text string
}

// wireCall is one request a scripted model received, as a provider would
// see it.
type wireCall struct {
	kind       string // "main", "fork" (tool choice none) or "compact" (structured summary)
	msgs       []wireMsg
	raw        string // JSON of the messages, for leak checks
	tools      []string
	toolChoice string
	maxTokens  int
	tokens     int
}

// text joins the rendered messages.
func (c *wireCall) text() string {
	var sb strings.Builder
	for _, m := range c.msgs {
		sb.WriteString(m.text)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// lastUser returns the text of the last user message.
func (c *wireCall) lastUser() string {
	for i := len(c.msgs) - 1; i >= 0; i-- {
		if c.msgs[i].role == message.RoleUser {
			return c.msgs[i].text
		}
	}
	return ""
}

// turn answers one ordinary model call.
type turn func(ctx context.Context, c *wireCall) (*model.ChatResponse, error)

// wireModel is a scripted chat model. Ordinary calls take the next scripted
// turn; summary forks and structured compaction calls are answered
// automatically. Every request is recorded.
type wireModel struct {
	t    *testing.T
	name string

	mu       sync.Mutex
	calls    []*wireCall
	turns    []turn
	forks    int
	forkText func(n int, c *wireCall) string
}

func newWireModel(t *testing.T, name string) *wireModel {
	return &wireModel{t: t, name: name}
}

// script queues turns for ordinary calls.
func (m *wireModel) script(turns ...turn) {
	m.mu.Lock()
	m.turns = append(m.turns, turns...)
	m.mu.Unlock()
}

// pending reports how many scripted turns are unused.
func (m *wireModel) pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.turns)
}

// requests returns the recorded calls of one kind ("" for all).
func (m *wireModel) requests(kind string) []*wireCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*wireCall
	for _, c := range m.calls {
		if kind == "" || c.kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func (m *wireModel) count(kind string) int { return len(m.requests(kind)) }

func renderBlocks(blocks []message.ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		switch v := b.(type) {
		case message.TextBlock:
			parts = append(parts, v.Text)
		case message.ToolCallBlock:
			parts = append(parts, "call "+v.Name+" "+v.Input)
		case message.ToolResultBlock:
			out := v.GetOutputText()
			if s, ok := v.Output.([]message.ContentBlock); ok {
				out = renderBlocks(s)
			}
			parts = append(parts, "result "+v.Name+": "+out)
		case message.ThinkingBlock:
			parts = append(parts, "thinking "+v.Thinking)
		}
	}
	return strings.Join(parts, "\n")
}

func (m *wireModel) capture(msgs []*message.Msg, opts []model.CallOption) *wireCall {
	var o model.CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	c := &wireCall{kind: "main"}
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		c.msgs = append(c.msgs, wireMsg{role: msg.Role, text: renderBlocks(msg.Content)})
	}
	raw, _ := json.Marshal(msgs)
	c.raw = string(raw)
	for _, s := range o.Tools {
		c.tools = append(c.tools, s.Function.Name)
		if s.Function.Name == "generate_structured_output" {
			c.kind = "compact"
		}
	}
	if o.ToolChoice != nil {
		c.toolChoice = o.ToolChoice.Mode
		if c.toolChoice == "none" && c.kind == "main" {
			c.kind = "fork"
		}
	}
	if o.MaxTokens != nil {
		c.maxTokens = *o.MaxTokens
	}
	c.tokens = m.CountTokens(msgs, o.Tools)
	return c
}

// Chat implements model.ChatModel.
func (m *wireModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	c := m.capture(msgs, opts)
	m.mu.Lock()
	m.calls = append(m.calls, c)
	switch c.kind {
	case "compact":
		m.mu.Unlock()
		input := `{"task_overview":"compacted overview","current_state":"s","important_discoveries":"d","next_steps":"n","context_to_preserve":"c"}`
		return &model.ChatResponse{
			Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "so", Name: "generate_structured_output", Input: input}},
			Usage:   &model.ChatUsage{InputTokens: c.tokens, OutputTokens: 40},
		}, nil
	case "fork":
		m.forks++
		n := m.forks
		text := fmt.Sprintf("%s summary %d: the user plans a Lisbon weekend at Hotel Aurora.", m.name, n)
		if m.forkText != nil {
			text = m.forkText(n, c)
		}
		m.mu.Unlock()
		return answer(c, fmt.Sprintf("%s-fork-%d", m.name, n), text), nil
	}
	if len(m.turns) == 0 {
		m.mu.Unlock()
		m.t.Errorf("%s: unexpected model call; last user message:\n%s", m.name, c.lastUser())
		return answer(c, m.name+"-unexpected", "(no scripted answer)"), nil
	}
	next := m.turns[0]
	m.turns = m.turns[1:]
	m.mu.Unlock()
	return next(ctx, c)
}

// ChatStream sends the text as one delta, then the final response.
func (m *wireModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	resp, err := m.Chat(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	ch := make(chan model.ChatResponse, 2)
	var deltas []message.ContentBlock
	for _, b := range resp.Content {
		if tb, ok := b.(message.TextBlock); ok {
			deltas = append(deltas, tb)
		}
	}
	if len(deltas) > 0 {
		ch <- model.ChatResponse{Content: deltas}
	}
	final := *resp
	final.IsLast = true
	ch <- final
	close(ch)
	return ch, nil
}

// CountTokens estimates (runes+3)/4 of the rendered messages and schemas.
func (m *wireModel) CountTokens(msgs []*message.Msg, tools []model.ToolSchema) int {
	n := 0
	for _, msg := range msgs {
		if msg != nil {
			n += len([]rune(renderBlocks(msg.Content))) + 8
		}
	}
	for _, s := range tools {
		n += len(s.Function.Name) + len(s.Function.Description) + len(s.Function.Parameters)
	}
	return (n + 3) / 4
}

// answer builds a response with text (when not empty) and tool calls, and a
// usage report.
func answer(c *wireCall, id, text string, calls ...message.ToolCallBlock) *model.ChatResponse {
	var blocks []message.ContentBlock
	out := 0
	if text != "" {
		blocks = append(blocks, message.TextBlock{Type: "text", ID: id, Text: text})
		out += len(text)/4 + 1
	}
	for _, tc := range calls {
		blocks = append(blocks, tc)
		out += len(tc.Input)/4 + 1
	}
	return &model.ChatResponse{Content: blocks, Usage: &model.ChatUsage{InputTokens: c.tokens, OutputTokens: out}}
}

var turnIDs struct {
	sync.Mutex
	n int
}

func nextID(prefix string) string {
	turnIDs.Lock()
	defer turnIDs.Unlock()
	turnIDs.n++
	return fmt.Sprintf("%s-%d", prefix, turnIDs.n)
}

// say answers with text only.
func say(text string) turn {
	return func(_ context.Context, c *wireCall) (*model.ChatResponse, error) {
		return answer(c, nextID("text"), text), nil
	}
}

// act answers with optional text and tool calls.
func act(text string, calls ...message.ToolCallBlock) turn {
	return func(_ context.Context, c *wireCall) (*model.ChatResponse, error) {
		return answer(c, nextID("text"), text, calls...), nil
	}
}

// inspect runs check on the request, then answers with next.
func inspect(check func(c *wireCall), next turn) turn {
	return func(ctx context.Context, c *wireCall) (*model.ChatResponse, error) {
		check(c)
		return next(ctx, c)
	}
}

func toolCall(name string, args map[string]any) message.ToolCallBlock {
	raw, _ := json.Marshal(args)
	return message.ToolCallBlock{Type: "tool_call", ID: nextID("call-" + name), Name: name, Input: string(raw), State: message.ToolCallPending}
}

// ---------------------------------------------------------------- events

// collector drains a Coordinator's Events channel.
type collector struct {
	mu     sync.Mutex
	events []thread.Event
	signal chan struct{}
	done   chan struct{}
}

// collect drains ch; with a gate it reads nothing until the gate closes.
func collect(ch <-chan thread.Event, gate <-chan struct{}) *collector {
	k := &collector{signal: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(k.done)
		if gate != nil {
			<-gate
		}
		for ev := range ch {
			k.mu.Lock()
			k.events = append(k.events, ev)
			close(k.signal)
			k.signal = make(chan struct{})
			k.mu.Unlock()
		}
	}()
	return k
}

func (k *collector) all() []thread.Event {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.events)
}

func (k *collector) len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.events)
}

// waitFor waits until ok holds for the collected events and returns them.
func (k *collector) waitFor(t *testing.T, what string, ok func([]thread.Event) bool) []thread.Event {
	t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		k.mu.Lock()
		evs := slices.Clone(k.events)
		sig := k.signal
		k.mu.Unlock()
		if ok(evs) {
			return evs
		}
		select {
		case <-sig:
		case <-k.done:
			k.mu.Lock()
			evs = slices.Clone(k.events)
			k.mu.Unlock()
			if ok(evs) {
				return evs
			}
			t.Fatalf("the event stream closed before %s", what)
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// ---------------------------------------------------------------- session

var (
	e2eEdgeDesc = thread.Descriptor{Name: "edge", Zone: thread.ZoneEdge, Description: "on-device assistant that controls the home devices",
		Window: 8192, SummaryBudget: 400, BriefMaxRunes: 2000, ToolGroups: []string{"device"}}
	e2eCloudDesc = thread.Descriptor{Name: "cloud", Zone: thread.ZoneCloud, Description: "cloud planner with a research team",
		Window: 200000, SummaryBudget: 0, ToolGroups: []string{"web", "files"}}
)

// memSaver keeps agent checkpoints in memory as JSON.
type memSaver struct {
	mu     sync.Mutex
	states map[string][]byte
}

func newMemSaver() *memSaver { return &memSaver{states: map[string][]byte{}} }

func (s *memSaver) SaveState(_ context.Context, id string, st *agent.AgentState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.states[id] = raw
	s.mu.Unlock()
	return nil
}

func (s *memSaver) LoadState(_ context.Context, id string) (*agent.AgentState, error) {
	s.mu.Lock()
	raw, ok := s.states[id]
	s.mu.Unlock()
	if !ok {
		return nil, nil
	}
	var st agent.AgentState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *memSaver) ListSessions(context.Context) ([]agent.SessionInfo, error) { return nil, nil }

func (s *memSaver) DeleteSession(_ context.Context, id string) error {
	s.mu.Lock()
	delete(s.states, id)
	s.mu.Unlock()
	return nil
}

// sessionOptions varies the session setup.
type sessionOptions struct {
	store       thread.Store
	cloudWindow int // the cloud's window and master ContextConfig.ContextSize; default 200000
	peerEvery   int
	// ownInit and ownEvery schedule the cloud's own notes (SummaryPolicy).
	ownInit, ownEvery int
	// wrapCloud and wrapEdge wrap a participant before the Coordinator gets it.
	wrapCloud func(tp *participant.TeamParticipant) thread.Participant
	wrapEdge  func(p *participant.AgentParticipant) thread.Participant
	// limits go to the Coordinator's configuration.
	limits thread.Limits
	// persistEdge gives the edge agent a StateSaver and restores it from its
	// checkpoint whenever the edge is built.
	persistEdge bool
	// eventGate makes the collector wait for it before reading any event.
	eventGate <-chan struct{}
	// masterOpts are added to the cloud master's options.
	masterOpts []agent.AgentOption
}

// session is one Main Thread over real agents.
type session struct {
	t      *testing.T
	ctx    context.Context
	store  thread.Store
	c      *thread.Coordinator
	events *collector
	base   uint64 // the log head when the current Coordinator started

	edge, master, member *wireModel
	edgeAgent            *agent.UnifiedAgent
	edgeP                *participant.AgentParticipant
	edgePart             thread.Participant // what the Coordinator drives for the edge
	cloudP               *participant.TeamParticipant
	cloudPart            thread.Participant // what the Coordinator drives for the cloud
	cloudDesc            thread.Descriptor
	descs                []thread.Descriptor
	saver                *memSaver
	evList               *thread.EventList
	opts                 sessionOptions

	mastersMu sync.Mutex
	masters   []*agent.UnifiedAgent

	sensorMu   sync.Mutex
	sensorGate chan struct{}
}

func objectSchema(props string, required ...string) json.RawMessage {
	req, _ := json.Marshal(required)
	if len(required) == 0 {
		req = []byte("[]")
	}
	return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":` + string(req) + `}`)
}

func newSession(t *testing.T, o *sessionOptions) *session {
	t.Helper()
	if o.store == nil {
		o.store = thread.NewMemoryStore()
	}
	if o.cloudWindow == 0 {
		o.cloudWindow = 200000
	}
	if o.peerEvery == 0 {
		// The scripted activations grow the cloud's input by about 100
		// tokens each.
		o.peerEvery = 80
	}
	s := &session{
		t: t, ctx: t.Context(), store: o.store, opts: *o,
		edge: newWireModel(t, "edge"), master: newWireModel(t, "master"), member: newWireModel(t, "member"),
		saver: newMemSaver(), evList: thread.NewEventList(thread.EventListOptions{}),
	}
	s.cloudDesc = e2eCloudDesc
	s.cloudDesc.Window = o.cloudWindow
	s.descs = []thread.Descriptor{e2eEdgeDesc, s.cloudDesc}
	s.buildEdge()
	s.buildCloud()
	s.start()
	return s
}

// rebuild builds new participants over the same models and checkpoints,
// the way a restarted process does, and starts a Coordinator over store.
func (s *session) rebuild(store thread.Store) {
	s.store = store
	s.buildEdge()
	s.buildCloud()
	s.start()
}

// buildEdge builds the edge: one agent with the thread tools and the device
// tools.
func (s *session) buildEdge() {
	t := s.t
	descs := s.descs
	// The session opts into the event list (event_read on the edge, capture
	// on the cloud), which is off by default.
	prep, err := participant.Prepare(e2eEdgeDesc, participant.PrepareOptions{Tools: []string{
		participant.ToolTransfer, participant.ToolAssignTask, participant.ToolReport, participant.ToolThreadRead, participant.ToolEventRead}})
	if err != nil {
		t.Fatal(err)
	}
	tk := tool.NewToolkit(prep.Tools()...)
	tk.AddGroup("device",
		tool.NewFunctionTool("device_control", "Switch a home device.", objectSchema(`"device":{"type":"string"},"state":{"type":"string"}`, "device", "state"),
			func(_ context.Context, in map[string]any) (any, error) {
				return fmt.Sprintf("%v is now %v", in["device"], in["state"]), nil
			}),
		tool.NewFunctionTool("door_lock", "Set the front door code.", objectSchema(`"code":{"type":"string"}`, "code"),
			func(_ context.Context, in map[string]any) (any, error) {
				return fmt.Sprintf("front door code set to %v", in["code"]), nil
			}),
		tool.NewFunctionTool("sensor_wait", "Wait for the motion sensor.", objectSchema(``),
			func(ctx context.Context, _ map[string]any) (any, error) {
				s.sensorMu.Lock()
				gate := s.sensorGate
				s.sensorMu.Unlock()
				if gate != nil {
					select {
					case <-gate:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return "no motion", nil
			}),
	)
	opts := []agent.AgentOption{
		agent.WithToolkit(tk),
		agent.WithPermissionContext(permission.NewContext(permission.ModeBypass)),
		agent.WithContextConfig(&agent.ContextConfig{ContextSize: 8192}),
	}
	if s.opts.persistEdge {
		st, err := agent.LoadCheckpoint(s.ctx, s.saver, "edge")
		if err != nil {
			st = &agent.AgentState{SessionID: "edge"}
		}
		opts = append(opts, agent.WithStateSaver(s.saver), agent.WithState(st))
	}
	s.edgeAgent = agent.NewUnifiedAgent("edge", "You run on the user's device.\n"+participant.SystemPromptSection(e2eEdgeDesc, descs), s.edge,
		append(opts, prep.Options()...)...)
	capture := true
	s.edgeP, err = prep.Participant(participant.AgentConfig{
		Descriptor: e2eEdgeDesc, Agent: s.edgeAgent, CaptureOutputs: &capture,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.edgePart = s.edgeP
	if s.opts.wrapEdge != nil {
		s.edgePart = s.opts.wrapEdge(s.edgeP)
	}
}

// buildCloud builds the cloud: a master with one member, rebuilt from its
// checkpoint for every activation. The participant reads the master's
// persisted state through TeamConfig.Load.
func (s *session) buildCloud() {
	t := s.t
	o := s.opts
	descs := s.descs
	var err error
	readFile := tool.NewFunctionTool("read_file", "Read a file.", objectSchema(`"path":{"type":"string"}`, "path"),
		func(_ context.Context, in map[string]any) (any, error) {
			return fmt.Sprintf("contents of %v:\nHotel Aurora, Rua Augusta 10, rated 9.1\nHotel Tejo, rated 7.4", in["path"]), nil
		})
	webFetch := tool.NewFunctionTool("web_fetch", "Fetch a web page.", objectSchema(`"url":{"type":"string"}`, "url"),
		func(_ context.Context, in map[string]any) (any, error) {
			return fmt.Sprintf("page %v: guests love Hotel Aurora.", in["url"]), nil
		})
	cloudSys := "You plan trips.\n" + participant.SystemPromptSection(s.cloudDesc, descs)
	captureCloud := true
	s.cloudP, err = participant.NewTeamParticipant(participant.TeamConfig{
		Descriptor: s.cloudDesc,
		Prepare: participant.PrepareOptions{Tools: []string{participant.ToolTransfer, participant.ToolAssignTask, participant.ToolReport,
			participant.ToolPublishArtifact, participant.ToolThreadRead, participant.ToolEventRead}},
		CaptureOutputs: &captureCloud,
		Master: func(ctx context.Context, assign tool.Tool, threadTools []tool.Tool, opts []agent.AgentOption) (*agent.UnifiedAgent, error) {
			mtk, err := pipeline.NewTeamLeaderToolkit(assign, append(threadTools, readFile, webFetch)...)
			if err != nil {
				return nil, err
			}
			st, err := agent.LoadCheckpoint(ctx, s.saver, "cloud-master")
			if err != nil {
				st = &agent.AgentState{SessionID: "cloud-master"}
			}
			all := append([]agent.AgentOption{
				agent.WithToolkit(mtk), agent.WithModelStreaming(false),
				agent.WithPermissionContext(permission.NewContext(permission.ModeBypass)),
				agent.WithStateSaver(s.saver), agent.WithState(st),
				agent.WithContextConfig(&agent.ContextConfig{ContextSize: o.cloudWindow}),
			}, opts...)
			all = append(all, o.masterOpts...)
			m := agent.NewUnifiedAgent("master", cloudSys, s.master, all...)
			s.mastersMu.Lock()
			s.masters = append(s.masters, m)
			s.mastersMu.Unlock()
			return m, nil
		},
		Members: []pipeline.TeamMember{{Name: "researcher", Description: "reads files and the web", Factory: func(context.Context) (*agent.UnifiedAgent, error) {
			return agent.NewUnifiedAgent("researcher", "You research.", s.member,
				agent.WithToolkit(tool.NewToolkit(readFile, webFetch)), agent.WithModelStreaming(false),
				agent.WithPermissionContext(permission.NewContext(permission.ModeBypass))), nil
		}}},
		Load: func(ctx context.Context) (*agent.AgentState, error) {
			return s.saver.LoadState(ctx, "cloud-master")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.cloudPart = s.cloudP
	if o.wrapCloud != nil {
		s.cloudPart = o.wrapCloud(s.cloudP)
	}
}

// start builds the router and a Coordinator over the session's store and
// participants.
func (s *session) start() {
	t := s.t
	o := s.opts
	router, err := route.New(route.Config{
		Mode: route.Hybrid, Edge: "edge", Cloud: "cloud",
		Classifier: classifier.NewFake(&classifier.FakeConfig{Response: classifier.Response{
			Model: "offline-fake",
			Answers: map[string]classifier.Answer{route.QuestionID: &classifier.ChoiceAnswer{
				Choice: "cloud", Confidence: 0.9, Probabilities: map[string]float64{"cloud": 0.9, "edge": 0.1},
			}},
		}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.base = s.head()
	s.c, err = thread.NewCoordinator(s.ctx, thread.Config{
		Store:        s.store,
		Participants: []thread.Participant{s.edgePart, s.cloudPart},
		Router:       router,
		Events:       s.evList,
		Summaries:    thread.SummaryPolicy{PeerEvery: o.peerEvery, OwnInit: o.ownInit, OwnEvery: o.ownEvery},
		Limits:       o.limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.events = collect(s.c.Events(), o.eventGate)
	c := s.c
	t.Cleanup(func() { _ = c.Close() })
}

// submit appends a query.
func (s *session) submit(q *thread.Query) uint64 {
	s.t.Helper()
	seq, err := s.c.Submit(s.ctx, *q)
	if err != nil {
		s.t.Fatalf("submit %q: %v", q.Text, err)
	}
	return seq
}

// settle waits until the Coordinator is idle and the collector holds every
// event it emitted up to then.
func (s *session) settle() {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	if err := s.c.WaitIdle(ctx); err != nil {
		s.t.Fatalf("wait idle: %v", err)
	}
	want := map[uint64]bool{}
	entries := s.since(s.base)
	for i := range entries {
		switch e := &entries[i]; e.Kind {
		case thread.KindOwnerChange, thread.KindMarker, thread.KindAssign, thread.KindReport, thread.KindSummary, thread.KindReply:
			want[e.Seq] = true
		}
	}
	s.events.waitFor(s.t, "the events of every entry and the idle event after them", func(evs []thread.Event) bool {
		seen := map[uint64]bool{}
		lastEntry := -1
		for i, ev := range evs {
			if ev.Kind == thread.EventEntry {
				seen[ev.Entry.Seq] = true
				lastEntry = i
			}
		}
		for seq := range want {
			if !seen[seq] {
				return false
			}
		}
		for i := lastEntry + 1; i < len(evs); i++ {
			if evs[i].Kind == thread.EventIdle {
				return true
			}
		}
		return false
	})
	for _, m := range []*wireModel{s.edge, s.master, s.member} {
		if n := m.pending(); n != 0 {
			s.t.Fatalf("%s has %d scripted answers left", m.name, n)
		}
	}
}

func (s *session) log() []thread.Entry {
	s.t.Helper()
	entries, err := s.store.Read(s.ctx, 0, thread.Filter{}, 0)
	if err != nil {
		s.t.Fatal(err)
	}
	return entries
}

// since returns the entries after seq.
func (s *session) since(seq uint64) []thread.Entry {
	var out []thread.Entry
	entries := s.log()
	for i := range entries {
		if entries[i].Seq > seq {
			out = append(out, entries[i])
		}
	}
	return out
}

func (s *session) head() uint64 {
	h, err := s.store.Head(s.ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	return h.LastSeq
}

// find returns the entries of kind in entries written by origin ("" for any).
func find(entries []thread.Entry, kind thread.Kind, origin string) []thread.Entry {
	var out []thread.Entry
	for i := range entries {
		if e := &entries[i]; e.Kind == kind && (origin == "" || e.Origin.Participant == origin) {
			out = append(out, *e)
		}
	}
	return out
}

func one(t *testing.T, entries []thread.Entry, what string) thread.Entry {
	t.Helper()
	if len(entries) != 1 {
		t.Fatalf("want one %s, got %d: %s", what, len(entries), describe(entries))
	}
	return entries[0]
}

func describe(entries []thread.Entry) string {
	var sb strings.Builder
	for i := range entries {
		e := &entries[i]
		fmt.Fprintf(&sb, "\n  #%d %s from %q: %q", e.Seq, e.Kind, e.Origin.String(), e.Text())
		if e.Owner != nil {
			fmt.Fprintf(&sb, " owner %s→%s (%s)", e.Owner.From, e.Owner.To, e.Owner.Reason)
		}
		if e.Marker != nil {
			fmt.Fprintf(&sb, " marker %s: %s", e.Marker.Kind, e.Marker.Detail)
		}
		if e.Coverage != nil {
			fmt.Fprintf(&sb, " coverage through %d excluding %v", e.Coverage.Through, e.Coverage.Excluded)
		}
	}
	return sb.String()
}

// agentEvents returns the agent events in evs.
func agentEvents(evs []thread.Event) []thread.Event {
	var out []thread.Event
	for _, ev := range evs {
		if ev.Kind == thread.EventAgent {
			out = append(out, ev)
		}
	}
	return out
}

// ledgerCount counts the ledger entries of participant, path and kind.
func ledgerCount(l *thread.Ledger, part, path string, kind thread.UsageKind) (calls, known int) {
	for _, e := range l.Entries() {
		if e.Participant == part && e.Path == path && e.Kind == kind {
			calls++
			if e.Usage != nil {
				known++
			}
		}
	}
	return calls, known
}

// ---------------------------------------------------------------- the session

func TestE2EMainThreadSession(t *testing.T) {
	s := newSession(t, &sessionOptions{})
	ledger := s.c.Ledger()

	// 1. A query the classifier routes to the cloud. The master asks
	// its member, which reads a file; only the master's text becomes the reply.
	s.master.script(
		act("Let me ask the researcher.", toolCall(pipeline.TeamAssignToolName, map[string]any{"member": "researcher", "prompt": "Find the best hotel in notes/hotels.md"})),
		say("Plan: stay at Hotel Aurora and visit Belem on Saturday."),
	)
	s.member.script(
		act("", toolCall("read_file", map[string]any{"path": "notes/hotels.md"})),
		say("MEMBER-NOTE: Hotel Aurora has the best rating."),
	)
	q1 := s.submit(&thread.Query{Text: "Plan a weekend in Lisbon for me."})
	s.settle()

	log := s.since(q1)
	oc1 := one(t, find(log, thread.KindOwnerChange, ""), "owner change for the first query")
	if oc1.Owner.Reason != thread.ReasonRouter || oc1.Owner.To != "cloud" || oc1.Owner.From != "" {
		t.Fatalf("first owner change: %s", describe([]thread.Entry{oc1}))
	}
	rep1 := one(t, find(log, thread.KindReply, "cloud"), "cloud reply")
	if rep1.Text() != "Let me ask the researcher.\nPlan: stay at Hotel Aurora and visit Belem on Saturday." || strings.Contains(rep1.Text(), "MEMBER-NOTE") {
		t.Fatalf("the reply must hold the master's text only: %q", rep1.Text())
	}
	if rep1.BasedOn != q1 {
		t.Fatalf("cloud reply: %s", describe([]thread.Entry{rep1}))
	}
	if d := rep1.Digest.Render(); !strings.Contains(d, pipeline.TeamAssignToolName) || !strings.Contains(d, `researcher/read_file {"path":"notes/hotels.md"}`) {
		t.Fatalf("digest = %q", d)
	}
	for _, ev := range agentEvents(s.events.all()) {
		if ev.Participant != "cloud" || !ev.Foreground {
			t.Fatalf("user event from a background activation: %+v", ev)
		}
	}
	if calls, known := ledgerCount(ledger, "cloud", "", thread.UsageMain); calls != 2 || known != 2 {
		t.Fatalf("master main usage: %d calls, %d known", calls, known)
	}
	if calls, known := ledgerCount(ledger, "cloud", "researcher", thread.UsageMain); calls != 2 || known != 2 {
		t.Fatalf("member main usage: %d calls, %d known", calls, known)
	}
	if calls, _ := ledgerCount(ledger, "router", "", thread.UsageClassifier); calls != 1 {
		t.Fatalf("classifier usage: %d calls", calls)
	}

	// 2. The cloud's growth passed PeerEvery: Fork B wrote a summary for the
	// edge from the master's last request.
	sum1 := one(t, find(log, thread.KindSummary, "cloud"), "peer summary")
	if sum1.Summary.Target != "edge" || sum1.Coverage.Through != rep1.Seq {
		t.Fatalf("peer summary: %s", describe([]thread.Entry{sum1}))
	}
	mains := s.master.requests("main")
	forks := s.master.requests("fork")
	if len(forks) != 1 {
		t.Fatalf("master forks = %d", len(forks))
	}
	checkFork(t, mains[len(mains)-1], forks[0], "Plan: stay at Hotel Aurora")
	if calls, known := ledgerCount(ledger, "cloud", "", thread.UsageForkPeer); calls != 1 || known != 1 {
		t.Fatalf("fork_peer usage: %d calls, %d known", calls, known)
	}

	// 3. The cloud hands the foreground to the edge with transfer alone. Its
	// text so far and the owner change land in one append; the master makes no
	// further call; the edge starts from the brief.
	mark := s.events.len()
	brief := map[string]any{"to": "edge", "goal": "Turn on the living room lamp", "done": []any{"Planned the Lisbon weekend"},
		"remaining": []any{"switch the living room lamp on"}}
	s.master.script(inspect(func(c *wireCall) {
		// The new master instance continues the previous one's context.
		if !strings.Contains(c.text(), "Plan: stay at Hotel Aurora and visit Belem on Saturday.") {
			t.Errorf("the second master lost the first activation's answer:\n%s", c.text())
		}
	}, act("That needs the device; handing over.", toolCall(participant.ToolTransfer, brief))))
	var outlineID string
	s.edge.script(
		inspect(func(c *wireCall) {
			in := c.lastUser()
			if !strings.HasPrefix(in, "[The foreground was handed to you by cloud]") || !strings.Contains(in, "Goal: Turn on the living room lamp") ||
				!strings.Contains(in, "- switch the living room lamp on") {
				t.Errorf("the edge input is not the brief: %q", in)
			}
		}, act("", toolCall("device_control", map[string]any{"device": "living room lamp", "state": "on"}))),
		act("", toolCall(participant.ToolEventRead, map[string]any{"action": "outline"})),
		func(ctx context.Context, c *wireCall) (*model.ChatResponse, error) {
			m := regexp.MustCompile(`(ev-\d+) read_file notes/hotels.md \(from cloud/researcher`).FindStringSubmatch(c.text())
			if m == nil {
				t.Errorf("the outline does not list the member's read_file:\n%s", c.text())
				return say("no record")(ctx, c)
			}
			outlineID = m[1]
			return act("", toolCall(participant.ToolEventRead, map[string]any{"action": "page", "id": outlineID}))(ctx, c)
		},
		inspect(func(c *wireCall) {
			if !strings.Contains(c.text(), "Hotel Aurora, Rua Augusta 10, rated 9.1") {
				t.Errorf("event_read page did not return the cloud's read_file output:\n%s", c.text())
			}
		}, say("EDGE-B: the living room lamp is on.")),
	)
	q2 := s.submit(&thread.Query{Text: "Turn on the living room lamp."})
	s.settle()

	log = s.since(q2)
	if got := find(log, thread.KindOwnerChange, ""); len(got) == 0 {
		t.Fatalf("no transfer:%s", describe(log))
	}
	transfer := one(t, find(log, thread.KindOwnerChange, "cloud"), "transfer")
	rep2 := one(t, find(log, thread.KindReply, "cloud"), "cloud reply sealed by the transfer")
	if transfer.Owner.Reason != thread.ReasonTransfer || transfer.Owner.To != "edge" || transfer.Owner.Brief == nil ||
		rep2.Seq+1 != transfer.Seq || rep2.Text() != "That needs the device; handing over." || rep2.BasedOn != q2 || transfer.BasedOn != q2 {
		t.Fatalf("the reply and the owner change must be one append:%s", describe(log))
	}
	if n := s.master.count("main"); n != 3 {
		t.Fatalf("the master made %d ordinary calls; a handed-off reply makes none after the transfer", n)
	}
	for _, e := range log {
		if e.Kind == thread.KindMarker {
			t.Fatalf("unexpected marker:%s", describe([]thread.Entry{e}))
		}
	}
	if owner, _ := s.c.Owner(); owner != "edge" {
		t.Fatalf("owner = %s", owner)
	}
	repB := one(t, find(log, thread.KindReply, "edge"), "edge reply")
	if repB.Text() != "EDGE-B: the living room lamp is on." {
		t.Fatalf("edge reply:%s", describe([]thread.Entry{repB}))
	}
	if d := repB.Digest.Render(); !strings.Contains(d, "device_control") || strings.Count(d, "event_read") != 2 {
		t.Fatalf("edge digest = %q", d)
	}
	// Nothing of the cloud reaches the user after the transfer committed.
	evs := s.events.all()[mark:]
	committed := false
	for _, ev := range evs {
		if ev.Kind == thread.EventEntry && ev.Entry.Seq == transfer.Seq {
			committed = true
		}
		if committed && ev.Kind == thread.EventAgent && ev.Participant == "cloud" {
			t.Fatalf("a cloud event reached the user after the transfer: %+v", ev)
		}
	}
	if !committed {
		t.Fatal("the transfer was not shown to the user")
	}

	// 5a. A query the user names for the edge stays there, and the edge uses
	// its device tool; the classifier is not asked.
	classifierCalls, _ := ledgerCount(ledger, "router", "", thread.UsageClassifier)
	s.edge.script(
		act("", toolCall("door_lock", map[string]any{"code": doorCode})),
		say("Done: the front door code is now "+doorCode+"."),
	)
	q3 := s.submit(&thread.Query{Text: "Set the code " + doorCode + " on the front door lock.", Explicit: "edge"})
	s.settle()
	log = s.since(q3)
	if got := find(log, thread.KindOwnerChange, ""); len(got) != 0 {
		t.Fatalf("a query named for the owner must not move the foreground:%s", describe(got))
	}
	repC := one(t, find(log, thread.KindReply, "edge"), "edge reply to the door query")
	if repC.BasedOn != q3 {
		t.Fatalf("edge reply:%s", describe([]thread.Entry{repC}))
	}
	if calls, _ := ledgerCount(ledger, "router", "", thread.UsageClassifier); calls != classifierCalls {
		t.Fatal("an explicit choice decides the route; the classifier must not be asked")
	}

	// 5b and 7. A query goes to the cloud. Its import holds the edge's
	// entries since the cloud's last activity, and event_read lists the
	// edge's tool outputs.
	s.master.script(
		inspect(func(c *wireCall) {
			text := c.text()
			for _, want := range []string{"EDGE-B: the living room lamp is on.", "Set the code " + doorCode, "Done: the front door code is now " + doorCode} {
				if !strings.Contains(text, want) {
					t.Errorf("the cloud import misses %q:\n%s", want, text)
				}
			}
		}, act("", toolCall(participant.ToolEventRead, map[string]any{"action": "outline"}))),
		inspect(func(c *wireCall) {
			if !regexp.MustCompile(`ev-\d+ door_lock[^\n]*\(from edge`).MatchString(c.text()) {
				t.Errorf("the outline does not list the edge's door_lock:\n%s", c.text())
			}
		}, say("FLIGHT: TP1350 leaves Porto on Friday at 09:00 and lands in Lisbon at 09:55; I would book the window seat.")),
	)
	q4 := s.submit(&thread.Query{Text: "Find me a flight to Lisbon on Friday."})
	s.settle()
	log = s.since(q4)
	oc4 := one(t, find(log, thread.KindOwnerChange, ""), "owner change for the flight query")
	if oc4.Owner.Reason != thread.ReasonRouter || oc4.Owner.From != "edge" || oc4.Owner.To != "cloud" {
		t.Fatalf("flight query owner change:%s", describe([]thread.Entry{oc4}))
	}
	repD := one(t, find(log, thread.KindReply, "cloud"), "cloud reply to the flight query")
	sumD := find(log, thread.KindSummary, "cloud")
	if len(sumD) == 0 {
		t.Fatalf("the flight query wrote no peer summary:%s", describe(log))
	}
	s3 := sumD[len(sumD)-1]
	if s3.Coverage.Through != repD.Seq {
		t.Fatalf("peer summary coverage:%s", describe([]thread.Entry{s3}))
	}

	// 4 and 6. A query that needs the device goes to the edge. The cloud's
	// summary covers entries past the edge's last activity, so the edge is
	// cleared: its request holds the summary, not its earlier context. The edge then assigns a
	// background task to the cloud; the report reaches it at its next
	// boundary while it is still running.
	mark = s.events.len()
	gate := make(chan struct{})
	s.sensorMu.Lock()
	s.sensorGate = gate
	s.sensorMu.Unlock()
	s.edge.script(
		inspect(func(c *wireCall) {
			text := c.text()
			if !strings.Contains(text, fmt.Sprintf("[Summary from cloud, covering the shared thread through #%d]:", repD.Seq)) {
				t.Errorf("the edge request has no peer summary:\n%s", text)
			}
			for _, old := range []string{"EDGE-B", "[The foreground was handed to you by cloud]", "call door_lock", "call device_control"} {
				if strings.Contains(text, old) {
					t.Errorf("the cleared edge still holds %q:\n%s", old, text)
				}
			}
		}, act("", toolCall(participant.ToolAssignTask, map[string]any{"to": "cloud", "task": "Look up the Lisbon Oceanarium opening hours.", "notify": true}))),
		act("", toolCall("sensor_wait", map[string]any{})),
		inspect(func(c *wireCall) {
			if !strings.Contains(c.text(), "report from cloud to edge") || !strings.Contains(c.text(), "TASK: The Oceanarium opens at 10:00 every day.") {
				t.Errorf("the report did not reach the edge's model input:\n%s", c.text())
			}
		}, say("EDGE-E: you picked Hotel Aurora, and the Oceanarium opens at 10:00.")),
	)
	s.master.script(inspect(func(c *wireCall) {
		if !strings.Contains(c.lastUser(), "[Task t-1 from edge]") || !strings.Contains(c.lastUser(), "Look up the Lisbon Oceanarium opening hours.") {
			t.Errorf("task input = %q", c.lastUser())
		}
	}, say("TASK: The Oceanarium opens at 10:00 every day.")))
	q5 := s.submit(&thread.Query{Text: "Remind me which hotel we picked, and have the cloud look up the aquarium hours.",
		NeedsGroups: []string{"device"}})
	s.events.waitFor(t, "the task report", func(evs []thread.Event) bool {
		for _, ev := range evs[mark:] {
			if ev.Kind == thread.EventEntry && ev.Entry.Kind == thread.KindReport && ev.Entry.Origin.Participant == "cloud" {
				return true
			}
		}
		return false
	})
	close(gate)
	s.settle()
	log = s.since(q5)
	oc5 := one(t, find(log, thread.KindOwnerChange, ""), "owner change for the device query")
	if oc5.Owner.To != "edge" || oc5.Owner.Reason != thread.ReasonRouter {
		t.Fatalf("device query owner change:%s", describe([]thread.Entry{oc5}))
	}
	assign := one(t, find(log, thread.KindAssign, "edge"), "assign")
	report := one(t, find(log, thread.KindReport, "cloud"), "task report")
	if assign.Task.ID != "t-1" || !assign.Task.Notify || !slices.Equal(assign.To, []string{"cloud"}) {
		t.Fatalf("assign:%s", describe([]thread.Entry{assign}))
	}
	if report.Task == nil || report.Task.ID != "t-1" || !report.Notify || !slices.Equal(report.To, []string{"edge"}) || report.BasedOn != assign.Seq ||
		report.Text() != "TASK: The Oceanarium opens at 10:00 every day." {
		t.Fatalf("report:%s", describe([]thread.Entry{report}))
	}
	repE := one(t, find(log, thread.KindReply, "edge"), "edge reply")
	if repE.Text() != "EDGE-E: you picked Hotel Aurora, and the Oceanarium opens at 10:00." {
		t.Fatalf("edge reply:%s", describe([]thread.Entry{repE}))
	}
	for _, ev := range agentEvents(s.events.all()[mark:]) {
		if ev.Participant != "edge" || !ev.Foreground {
			t.Fatalf("the background task's events reached the user: %+v", ev)
		}
	}
	if n := s.edge.count("main"); n != 9 {
		t.Fatalf("edge main calls = %d; the report must arrive at the boundary, not as a wake", n)
	}

	// 6b. Another background task whose report arrives after the edge went
	// idle: the report wakes the edge, which answers it in the foreground.
	gate = make(chan struct{})
	s.edge.script(
		act("", toolCall(participant.ToolAssignTask, map[string]any{"to": "cloud", "task": "Get tomorrow's weather in Lisbon.", "notify": true})),
		say("EDGE-E2: I asked the cloud for the weather."),
		inspect(func(c *wireCall) {
			if !strings.Contains(c.lastUser(), "report from cloud to edge] TASK: Tomorrow is sunny, 24 C.") {
				t.Errorf("the wake input is not the report: %q", c.lastUser())
			}
		}, say("EDGE-WAKE: tomorrow is sunny.")),
	)
	s.master.script(func(ctx context.Context, c *wireCall) (*model.ChatResponse, error) {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return say("TASK: Tomorrow is sunny, 24 C.")(ctx, c)
	})
	mark = s.events.len()
	q5b := s.submit(&thread.Query{Text: "Ask the cloud about tomorrow's weather.", NeedsGroups: []string{"device"}})
	s.events.waitFor(t, "the edge reply before the report", func(evs []thread.Event) bool {
		for _, ev := range evs[mark:] {
			if ev.Kind == thread.EventEntry && ev.Entry.Kind == thread.KindReply && ev.Entry.Origin.Participant == "edge" {
				return true
			}
		}
		return false
	})
	close(gate)
	s.settle()
	log = s.since(q5b)
	report2 := one(t, find(log, thread.KindReport, "cloud"), "second task report")
	edgeReplies := find(log, thread.KindReply, "edge")
	if len(edgeReplies) != 2 || edgeReplies[0].Seq > report2.Seq || edgeReplies[1].BasedOn != report2.Seq ||
		edgeReplies[1].Text() != "EDGE-WAKE: tomorrow is sunny." {
		t.Fatalf("the report must wake the idle edge:%s", describe(log))
	}

	// 8. The cloud fetches a web page, then transfers back to the edge.
	s.master.script(
		act("", toolCall("web_fetch", map[string]any{"url": "https://reviews.example/aurora"})),
		act("Guests love Hotel Aurora; the device will tell you more.", toolCall(participant.ToolTransfer, map[string]any{
			"to": "edge", "goal": "Tell the user what guests say about Hotel Aurora", "remaining": []any{"share the review summary"}})),
	)
	s.edge.script(say("EDGE-F: guests love Hotel Aurora."))
	q6 := s.submit(&thread.Query{Text: "Check online reviews for Hotel Aurora."})
	s.settle()
	log = s.since(q6)
	one(t, find(log, thread.KindReply, "cloud"), "cloud reply before the transfer")
	tr6 := one(t, find(log, thread.KindOwnerChange, "cloud"), "transfer")
	repF := one(t, find(log, thread.KindReply, "edge"), "edge reply after the transfer")
	if tr6.Owner.To != "edge" || repF.Text() != "EDGE-F: guests love Hotel Aurora." {
		t.Fatalf("the transfer back to the edge:%s", describe(log))
	}

	// 5c. A last query goes to the cloud.
	s.master.script(inspect(func(c *wireCall) {
		if !strings.Contains(c.text(), "EDGE-F: guests love Hotel Aurora.") {
			t.Errorf("the cloud import misses the edge's reply:\n%s", c.text())
		}
	}, say("SUMMARY: Hotel Aurora, flight TP1350, Oceanarium at 10:00.")))
	s.submit(&thread.Query{Text: "Thanks. Sum up the plan."})
	s.settle()

	for _, ev := range s.events.all() {
		if ev.Kind == thread.EventError {
			t.Fatalf("error event: %v", ev.Err)
		}
	}

	t.Logf("log:%s", describe(s.log()))
	for p, tot := range ledger.Totals() {
		t.Logf("ledger %q: %+v", p, tot)
	}
	for k, tot := range ledger.TotalsByKind() {
		t.Logf("ledger kind %s: %+v", k, tot)
	}
}

// forkObserver wraps the cloud participant to record when its forks return
// and when the Coordinator offers its own notes.
type forkObserver struct {
	*participant.TeamParticipant
	store thread.Store

	mu    sync.Mutex
	notes []string
}

func (o *forkObserver) note(s string) {
	o.mu.Lock()
	o.notes = append(o.notes, s)
	o.mu.Unlock()
}

func (o *forkObserver) seen() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.notes)
}

func (o *forkObserver) Fork(ctx context.Context, snap thread.Snapshot, spec thread.ForkSpec) (thread.ForkOutput, error) {
	out, err := o.TeamParticipant.Fork(ctx, snap, spec)
	kind := "own"
	if strings.Contains(spec.Prompt, "for edge") {
		kind = "peer"
	}
	o.note("fork " + kind)
	return out, err
}

func (o *forkObserver) OfferOwnSummary(ctx context.Context, snap thread.Snapshot, text string) error {
	sums, err := o.store.Read(ctx, 0, thread.Filter{Kinds: []thread.Kind{thread.KindSummary}}, 0)
	if err != nil {
		return err
	}
	o.note(fmt.Sprintf("offer own after %d summaries", len(sums)))
	return o.TeamParticipant.OfferOwnSummary(ctx, snap, text)
}

// TestE2EOwnSummary covers Fork A the way Claude Code's session memory
// works: once the cloud's input reaches OwnInit, notes forked from its last
// request are offered to the master after the peer fork of the same job
// settled. The master is rebuilt from its checkpoint for the next request
// and installs the notes when its context reaches its compaction line,
// without a compaction call, so that request starts with them.
func TestE2EOwnSummary(t *testing.T) {
	store := thread.NewMemoryStore()
	var obs *forkObserver
	s := newSession(t, &sessionOptions{store: store, cloudWindow: 4000, ownInit: 2000, ownEvery: 1000,
		wrapCloud: func(tp *participant.TeamParticipant) thread.Participant {
			obs = &forkObserver{TeamParticipant: tp, store: store}
			return obs
		}})
	const notes = "OWN-NOTES: the user is planning Lisbon in spring and sent a long itinerary."
	s.master.forkText = func(n int, c *wireCall) string {
		if strings.Contains(c.lastUser(), "Write notes on this conversation") {
			return notes
		}
		return fmt.Sprintf("PEER-SUMMARY %d: Lisbon in spring.", n)
	}
	ledger := s.c.Ledger()

	s.master.script(say("A1: Lisbon in spring is mild and sunny."))
	s.submit(&thread.Query{Text: "What is Lisbon like in spring?"})
	s.settle()
	st := s.cloudP.State()
	if st.Window != 4000 || st.CompactAt != 3200 || st.InputTokens >= 2000 {
		t.Fatalf("after the first query the cloud should be under OwnInit: %+v", st)
	}
	if calls, _ := ledgerCount(ledger, "cloud", "", thread.UsageForkOwn); calls != 0 {
		t.Fatalf("no own notes yet: %d", calls)
	}

	long := "Here is my draft itinerary: " + strings.Repeat("a day trip to Sintra, lunch in Cascais, dinner in Alfama; ", 80)
	s.master.script(say("A2: the itinerary is too packed; drop Cascais."))
	s.submit(&thread.Query{Text: long})
	s.settle()
	st = s.cloudP.State()
	if st.InputTokens < 2000 || st.InputTokens >= st.CompactAt {
		t.Fatalf("after the long query the cloud should be between OwnInit and its compaction line: %+v", st)
	}
	if calls, known := ledgerCount(ledger, "cloud", "", thread.UsageForkOwn); calls != 1 || known != 1 {
		t.Fatalf("fork_own usage: %d calls, %d known (state %+v)", calls, known, st)
	}
	summaries := find(s.log(), thread.KindSummary, "cloud")
	seen := obs.seen()
	want := fmt.Sprintf("offer own after %d summaries", len(summaries))
	if len(seen) < 3 || seen[len(seen)-1] != want || !slices.Contains(seen[len(seen)-3:], "fork peer") || !slices.Contains(seen[len(seen)-3:], "fork own") {
		t.Fatalf("the notes must be offered after the job's peer summary was appended: %v (want last %q)", seen, want)
	}
	if after := s.cloudP.State(); after.InputTokens != st.InputTokens {
		t.Fatalf("an offer must not change the context before the compaction line: %+v", after)
	}

	more := "Here is my packing list: " + strings.Repeat("a rain jacket, walking shoes, a phrasebook, sunscreen; ", 80)
	s.master.script(inspect(func(c *wireCall) {
		if len(c.msgs) < 3 || c.msgs[0].role != message.RoleSystem || c.msgs[1].text != "[Previous context summary]: "+notes {
			t.Errorf("the request must start with the own notes:\n%s", c.text())
		}
		text := c.text()
		for _, keep := range []string{"A2: the itinerary is too packed; drop Cascais.", "Here is my packing list"} {
			if !strings.Contains(text, keep) {
				t.Errorf("the recent tail is missing %q:\n%s", keep, text)
			}
		}
		for _, old := range []string{"A1: Lisbon in spring", "What is Lisbon like in spring?", "Here is my draft itinerary"} {
			if strings.Contains(text, old) {
				t.Errorf("the summarized context still holds %q:\n%s", old, text)
			}
		}
	}, say("A3: pack light; next, book the hotel.")))
	s.submit(&thread.Query{Text: more})
	s.settle()
	if n := s.master.count("compact"); n != 0 {
		t.Fatalf("the notes were not installed: %d compaction calls", n)
	}
	if after := s.cloudP.State(); after.InputTokens >= st.CompactAt {
		t.Fatalf("the notes did not shrink the cloud's input: %+v", after)
	}
}

// checkFork asserts that fork is last plus the reply's own answer and one
// appended user message, with the same tools, tool choice none and a token
// cap.
func checkFork(t *testing.T, last, fork *wireCall, answerText string) {
	t.Helper()
	n := len(last.msgs)
	if len(fork.msgs) != n+2 {
		t.Fatalf("fork request has %d messages, the last request %d", len(fork.msgs), n)
	}
	for i := range last.msgs {
		if fork.msgs[i] != last.msgs[i] {
			t.Fatalf("fork message %d differs from the last request:\n%q\n%q", i, fork.msgs[i].text, last.msgs[i].text)
		}
	}
	if m := fork.msgs[n]; m.role != message.RoleAssistant || !strings.Contains(m.text, answerText) {
		t.Fatalf("fork message %d should be the answer to the last request: %+v", n, m)
	}
	if m := fork.msgs[n+1]; m.role != message.RoleUser || !strings.Contains(m.text, "Write a summary of this conversation for edge") {
		t.Fatalf("fork prompt: %+v", m)
	}
	if !slices.Equal(fork.tools, last.tools) || fork.toolChoice != "none" || fork.maxTokens <= 0 {
		t.Fatalf("fork tools %v (last %v), choice %q, max tokens %d", fork.tools, last.tools, fork.toolChoice, fork.maxTokens)
	}
}

// TestE2EFileStoreRecovery stops a session while an activation is blocked,
// the way a crashed process stops: the log is closed before the activation
// can seal anything. A new Coordinator over the reopened log and the same
// participants resumes the request on its owner: the input quotes it and
// warns that work done before may or may not have taken effect, the
// restored master holds its history once, and the next query carries on.
func TestE2EFileStoreRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread.jsonl")
	first, err := thread.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(t, &sessionOptions{store: first})

	s.master.script(say("R1: Lisbon in May is mild."))
	s.submit(&thread.Query{Text: "What is Lisbon like in May?"})
	s.settle()

	entered := make(chan struct{})
	s.master.script(func(ctx context.Context, _ *wireCall) (*model.ChatResponse, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	blocked := s.submit(&thread.Query{Text: "Book the Hotel Aurora for Friday."})
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the master never started on the blocked query")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.c.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := thread.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	s.store = second
	if before := s.head(); before != blocked {
		t.Fatalf("the crashed session wrote past the blocked input #%d:%s", blocked, describe(s.since(blocked)))
	}
	// A restarted process builds its participants anew; the new cloud
	// participant reads the master's persisted state, which holds the
	// blocked input. The resumed request is the master's next call.
	s.master.script(inspect(func(c *wireCall) {
		text := c.text()
		note := fmt.Sprintf(`[The session restarted while the user's request #%d "Book the Hotel Aurora for Friday." was being answered`, blocked)
		if !strings.Contains(text, note) || !strings.Contains(text, "check the actual state before you redo anything") {
			t.Errorf("the resumed request misses the restart note:\n%s", text)
		}
		// The blocked input once as the persisted message, once quoted.
		for once, n := range map[string]int{"What is Lisbon like in May?": 1, "R1: Lisbon in May is mild.": 1, "Book the Hotel Aurora for Friday.": 2} {
			if got := strings.Count(text, once); got != n {
				t.Errorf("the resumed request holds %q %d times, want %d:\n%s", once, got, n, text)
			}
		}
	}, say("R2: I could not confirm the booking; let me check before trying again.")))
	s.rebuild(second)
	s.settle()

	rec := one(t, find(s.since(blocked), thread.KindMarker, ""), "restarted marker")
	if rec.Marker.Kind != thread.MarkerRestarted || rec.BasedOn != blocked || !strings.Contains(rec.Marker.Detail, fmt.Sprintf("#%d", blocked)) {
		t.Fatalf("restarted marker:%s", describe([]thread.Entry{rec}))
	}
	resumed := one(t, find(s.since(blocked), thread.KindReply, "cloud"), "resumed reply")
	if resumed.BasedOn != rec.Seq {
		t.Fatalf("resumed reply:%s", describe([]thread.Entry{resumed}))
	}
	if owner, _ := s.c.Owner(); owner != "cloud" {
		t.Fatalf("owner after recovery = %s", owner)
	}

	s.master.script(say("R3: the booking is confirmed."))
	next := s.submit(&thread.Query{Text: "Did the booking work?"})
	s.settle()
	rep := one(t, find(s.since(next), thread.KindReply, "cloud"), "reply after recovery")
	if rep.BasedOn != next {
		t.Fatalf("reply after recovery:%s", describe([]thread.Entry{rep}))
	}
	if n := s.master.count("main"); n != 4 {
		t.Fatalf("master calls = %d; want the first query, the blocked call, the resumed one and the next", n)
	}
}

// TestE2EFailoverHandsOverProgress: the master publishes its plan with
// update_plan, then its model fails. The log keeps the plan as the run's
// progress and the failure as a marker; the edge's failover request holds
// both, and its input is one line pointing at them.
func TestE2EFailoverHandsOverProgress(t *testing.T) {
	s := newSession(t, &sessionOptions{masterOpts: []agent.AgentOption{agent.WithPlanTool()}})
	s.master.script(
		act("", toolCall("update_plan", map[string]any{"plan": []any{
			map[string]any{"step": "list hotels near Rua Augusta", "status": "completed"},
			map[string]any{"step": "compare their prices", "status": "in_progress"},
		}})),
		func(context.Context, *wireCall) (*model.ChatResponse, error) {
			return nil, errors.New("invalid api key")
		},
	)
	var input string
	s.edge.script(inspect(func(c *wireCall) {
		text := c.text()
		input = c.msgs[len(c.msgs)-1].text
		if !strings.Contains(text, "2. [in_progress] compare their prices") || !strings.Contains(text, "marker failed] cloud failed: participant cloud: invalid api key") {
			t.Errorf("the edge's request lacks the progress or the failure:\n%s", text)
		}
	}, say("E1: Hotel Aurora is the better price.")))
	q := s.submit(&thread.Query{Text: "Find me a hotel in Lisbon.", Explicit: "cloud"})
	s.settle()

	prog := one(t, find(s.since(q), thread.KindProgress, "cloud"), "cloud progress")
	var failed thread.Entry
	for _, e := range find(s.since(q), thread.KindMarker, "") {
		if e.Marker.Kind == thread.MarkerFailed {
			failed = e
		}
	}
	if failed.BasedOn != q || prog.BasedOn != q {
		t.Fatalf("progress and failure:%s", describe([]thread.Entry{prog, failed}))
	}
	want := fmt.Sprintf(`[cloud failed: participant cloud: invalid api key (#%d). You now answer the user: continue their request #%d "Find me a hotel in Lisbon." `+
		`from where cloud left off. What cloud did is in the summary and the entries above; its latest progress is #%d.]`, failed.Seq, q, prog.Seq)
	if input != want {
		t.Fatalf("failover input:\n%s\nwant\n%s", input, want)
	}
	rep := one(t, find(s.since(q), thread.KindReply, "edge"), "edge reply")
	if rep.Text() != "E1: Hotel Aurora is the better price." {
		t.Fatalf("edge reply:%s", describe([]thread.Entry{rep}))
	}
}
