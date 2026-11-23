// This offline example runs a short Main Thread session: an edge agent on the
// user's device and a cloud team (a master with one researcher) share one
// append-only log through a thread.Coordinator. Every model is scripted, the
// routing classifier is the offline fake and nothing touches the network, so
// the session and its output are the same on every run.
//
// The session shows a query answered in the cloud with a sub-agent, a
// transfer of the foreground to the edge, a query the user names for the
// edge, and a background task whose report wakes the edge. It ends by
// printing the log, the owner and the ledger totals.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/pipeline"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/storage"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread/participant"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread/route"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// bedroomTemp is the temperature the user has the edge set on the bedroom
// thermostat.
const bedroomTemp = "21"

var (
	edgeDesc = thread.Descriptor{Name: "edge", Zone: thread.ZoneEdge, Description: "on-device assistant that controls the home",
		Window: 8192, SummaryBudget: 400, BriefMaxRunes: 2000, ToolGroups: []string{"device"}}
	cloudDesc = thread.Descriptor{Name: "cloud", Zone: thread.ZoneCloud, Description: "cloud trip planner with a research team",
		Window: 200000}
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	err := run(ctx, os.Stdout)
	cancel()
	if err != nil {
		log.Fatal(err)
	}
}

// run plays the session and writes its report to w.
func run(ctx context.Context, w io.Writer) error {
	s, err := newSession(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.c.Close() }()

	fmt.Fprintln(w, "Offline Main Thread session: every model is scripted; nothing leaves this process.")
	steps := []struct {
		q    thread.Query
		note string
	}{
		{thread.Query{Text: "Plan a weekend in Lisbon."}, "routed"},
		{thread.Query{Text: "Turn on the living room lamp."}, "routed"},
		{thread.Query{Text: "Set the bedroom thermostat to " + bedroomTemp + " degrees.", Explicit: "edge"}, "for the edge"},
		{thread.Query{Text: "Dim the lights and have the cloud look up the Oceanarium opening hours.",
			NeedsGroups: []string{"device"}}, "needs the device"},
	}
	for i := range steps {
		st := &steps[i]
		fmt.Fprintf(w, "> user (%s): %s\n", st.note, st.q.Text)
		if err := s.ask(ctx, &st.q); err != nil {
			return err
		}
	}
	if err := s.problems(); err != nil {
		return err
	}
	return s.report(ctx, w)
}

// session holds the Coordinator and the scripted pieces around it.
type session struct {
	store   *thread.MemoryStore
	c       *thread.Coordinator
	watch   *watcher
	edge    *fakeModel
	master  *fakeModel
	members *fakeModel
}

func newSession(ctx context.Context) (*session, error) {
	s := &session{store: thread.NewMemoryStore(), watch: &watcher{}}
	s.edge = &fakeModel{name: "edge", respond: s.edgeTurn}
	s.master = &fakeModel{name: "master", respond: s.masterTurn}
	s.members = &fakeModel{name: "researcher", respond: researcherTurn}
	descs := []thread.Descriptor{edgeDesc, cloudDesc}

	// The edge: one agent with the thread tools and the device tools.
	prep, err := participant.Prepare(edgeDesc, participant.PrepareOptions{})
	if err != nil {
		return nil, err
	}
	tk := tool.NewToolkit(prep.Tools()...)
	tk.AddGroup("device",
		tool.NewFunctionTool("device_control", "Switch a home device.", schema(`"device":{"type":"string"},"state":{"type":"string"}`, "device", "state"),
			func(_ context.Context, in map[string]any) (any, error) {
				return fmt.Sprintf("%v is now %v", in["device"], in["state"]), nil
			}),
		tool.NewFunctionTool("thermostat", "Set a thermostat's target temperature in degrees Celsius.",
			schema(`"room":{"type":"string"},"celsius":{"type":"string"}`, "room", "celsius"),
			func(_ context.Context, in map[string]any) (any, error) {
				return fmt.Sprintf("%v thermostat set to %v degrees", in["room"], in["celsius"]), nil
			}),
	)
	// Bypass mode lets the scripted tool calls run without prompts; a real
	// host asks its user instead.
	edgeAgent := agent.NewUnifiedAgent("edge", "You run on the user's device.\n"+participant.SystemPromptSection(edgeDesc, descs), s.edge,
		append([]agent.AgentOption{
			agent.WithToolkit(tk),
			agent.WithModelStreaming(false),
			agent.WithPermissionContext(permission.NewContext(permission.ModeBypass)),
			// The own summary shares the local summary budget with the peer
			// summary the cloud writes for this window.
			agent.WithContextConfig(&agent.ContextConfig{ContextSize: edgeDesc.Window, ThreadSummaryTokens: 1500}),
		}, prep.Options()...)...)
	edgeP, err := prep.Participant(participant.AgentConfig{Descriptor: edgeDesc, Agent: edgeAgent})
	if err != nil {
		return nil, err
	}

	// The cloud: a master rebuilt from its checkpoint for every activation,
	// with one researcher that reads files. A host that restores a session
	// also sets TeamConfig.Load, so the participant reports its cursor before
	// its first activation; this session starts new.
	readFile := tool.NewFunctionTool("read_file", "Read a file.", schema(`"path":{"type":"string"}`, "path"),
		func(_ context.Context, in map[string]any) (any, error) {
			return fmt.Sprintf("%v:\nHotel Aurora, Rua Augusta 10, rated 9.1\nHotel Tejo, rated 7.4", in["path"]), nil
		})
	saver := storage.NewInMemoryStorage()
	// built is set when the first master is built; later builds load the
	// checkpoint its run saved.
	var built atomic.Bool
	cloudSys := "You plan trips.\n" + participant.SystemPromptSection(cloudDesc, descs)
	cloudP, err := participant.NewTeamParticipant(participant.TeamConfig{
		Descriptor: cloudDesc,
		Master: func(ctx context.Context, assign tool.Tool, threadTools []tool.Tool, opts []agent.AgentOption) (*agent.UnifiedAgent, error) {
			mtk, err := pipeline.NewTeamLeaderToolkit(assign, threadTools...)
			if err != nil {
				return nil, err
			}
			st := &agent.AgentState{SessionID: "cloud-master"}
			if built.Swap(true) {
				if st, err = agent.LoadCheckpoint(ctx, saver, "cloud-master"); err != nil {
					return nil, err
				}
			}
			return agent.NewUnifiedAgent("master", cloudSys, s.master, append([]agent.AgentOption{
				agent.WithToolkit(mtk),
				agent.WithModelStreaming(false),
				agent.WithPermissionContext(permission.NewContext(permission.ModeBypass)),
				agent.WithStateSaver(saver),
				agent.WithState(st),
				agent.WithContextConfig(&agent.ContextConfig{ContextSize: cloudDesc.Window}),
			}, opts...)...), nil
		},
		Members: []pipeline.TeamMember{{Name: "researcher", Description: "reads the user's notes", Factory: func(context.Context) (*agent.UnifiedAgent, error) {
			return agent.NewUnifiedAgent("researcher", "You research.", s.members,
				agent.WithToolkit(tool.NewToolkit(readFile)),
				agent.WithModelStreaming(false),
				agent.WithPermissionContext(permission.NewContext(permission.ModeBypass))), nil
		}}},
	})
	if err != nil {
		return nil, err
	}

	// Hybrid routing with the offline classifier: it always prefers the
	// cloud, so only the user's explicit choice or a needed tool group keeps
	// a query on the device.
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
		return nil, err
	}
	s.c, err = thread.NewCoordinator(ctx, thread.Config{
		Store:        s.store,
		Participants: []thread.Participant{edgeP, cloudP},
		Router:       router,
		// Peer summaries are written only when the cloud hands the
		// foreground to the edge, which keeps the log short: the window
		// share rule is off and the growth rule never fires.
		Summaries: thread.SummaryPolicy{PeerEvery: 1 << 30, PeerRatio: -1},
	})
	if err != nil {
		return nil, err
	}
	go s.watch.run(s.c.Events())
	return s, nil
}

// ask submits q and waits until every activation and summary it caused has
// finished.
func (s *session) ask(ctx context.Context, q *thread.Query) error {
	if _, err := s.c.Submit(ctx, *q); err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.c.WaitIdle(wctx); err != nil {
		return fmt.Errorf("wait for %q: %w", q.Text, err)
	}
	return nil
}

// afterEdgeReply returns a channel closed once the edge's next reply is in
// the log. The scripted cloud turns wait on it so their entries land in the
// same order on every run.
func (s *session) afterEdgeReply() <-chan struct{} {
	head, _ := s.store.Head(context.Background())
	return s.watch.when(func(e *thread.Entry) bool {
		return e.Seq > head.LastSeq && e.Kind == thread.KindReply && e.Origin.Participant == "edge"
	})
}

// problems reports unexpected model requests and error events.
func (s *session) problems() error {
	var errs []error
	for _, m := range []*fakeModel{s.edge, s.master, s.members} {
		errs = append(errs, m.problems()...)
	}
	errs = append(errs, s.watch.problems()...)
	return errors.Join(errs...)
}

// report prints the log, the owner and the ledger totals.
func (s *session) report(ctx context.Context, w io.Writer) error {
	entries, err := s.store.Read(ctx, 0, thread.Filter{}, 0)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "\nLog:")
	for i := range entries {
		e := &entries[i]
		fmt.Fprintf(w, "  #%-2d %-12s %-11s %s\n", e.Seq, e.Kind, e.Origin, short(e))
	}
	owner, epoch := s.c.Owner()
	fmt.Fprintf(w, "\nOwner: %s (epoch %d)\n", owner, epoch)

	totals := s.c.Ledger().Totals()
	fmt.Fprintln(w, "\nLedger by participant:")
	for _, p := range sortedKeys(totals) {
		name := p
		if name == "" {
			name = "total"
		}
		fmt.Fprintf(w, "  %-12s %s\n", name, totalsLine(totals[p]))
	}
	byKind := s.c.Ledger().TotalsByKind()
	fmt.Fprintln(w, "\nLedger by kind:")
	for _, k := range sortedKeys(byKind) {
		fmt.Fprintf(w, "  %-12s %s\n", k, totalsLine(byKind[k]))
	}
	return nil
}

func totalsLine(t thread.LedgerTotals) string { //nolint:gocritic // a small value type
	return fmt.Sprintf("calls %d (unknown usage %d), input %d, output %d tokens", t.Calls, t.UnknownCalls, t.InputTokens, t.OutputTokens)
}

func sortedKeys[K ~string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// short is a one-line description of an entry.
func short(e *thread.Entry) string {
	var text string
	switch {
	case e.Kind == thread.KindOwnerChange && e.Owner != nil:
		from := e.Owner.From
		if from == "" {
			from = "none"
		}
		text = fmt.Sprintf("%s -> %s (%s)", from, e.Owner.To, e.Owner.Reason)
	case e.Kind == thread.KindSummary && e.Summary != nil && e.Coverage != nil:
		text = fmt.Sprintf("for %s, through #%d: %s", e.Summary.Target, e.Coverage.Through, e.Text())
	case e.Task != nil:
		text = fmt.Sprintf("task %s to %s: %s", e.Task.ID, strings.Join(e.To, ", "), e.Text())
	case e.Marker != nil:
		text = string(e.Marker.Kind) + ": " + e.Marker.Detail
	default:
		text = e.Text()
	}
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) > 72 {
		text = string([]rune(text)[:71]) + "…"
	}
	return text
}

func schema(props string, required ...string) json.RawMessage {
	req, _ := json.Marshal(required)
	return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":` + string(req) + `}`)
}

// ---------------------------------------------------------------- scripts

// edgeTurn answers the edge agent's requests.
func (s *session) edgeTurn(_ context.Context, r *request) *model.ChatResponse {
	switch r.result {
	case "device_control":
		if strings.Contains(r.input, "living room lamp") {
			return r.say("The living room lamp is on.")
		}
		return r.say("The lights are dimmed. I asked the cloud for the Oceanarium hours and will tell you when they arrive.")
	case "thermostat":
		return r.say("Done: the bedroom thermostat is set to " + bedroomTemp + " degrees.")
	case participant.ToolAssignTask:
		return r.call("", "device_control", map[string]any{"device": "living room lights", "state": "dimmed"})
	case "":
	default:
		return r.unexpected()
	}
	switch {
	case strings.Contains(r.input, "handed to you by cloud"):
		return r.call("", "device_control", map[string]any{"device": "living room lamp", "state": "on"})
	case strings.Contains(r.input, "thermostat"):
		return r.call("", "thermostat", map[string]any{"room": "bedroom", "celsius": bedroomTemp})
	case strings.Contains(r.input, "report from cloud"):
		return r.say("The cloud says the Oceanarium opens at 10:00 every day.")
	case strings.Contains(r.input, "Oceanarium"):
		// The cloud's task waits until this activation has answered, so its
		// report arrives while the edge is idle and wakes it.
		s.master.setGate(s.afterEdgeReply())
		return r.call("", participant.ToolAssignTask, map[string]any{
			"to": "cloud", "task": "Look up the Lisbon Oceanarium opening hours.", "notify": true,
		})
	}
	return r.unexpected()
}

// masterTurn answers the cloud master's requests, including its summary
// forks.
func (s *session) masterTurn(ctx context.Context, r *request) *model.ChatResponse {
	if r.fork {
		if err := s.master.waitGate(ctx); err != nil {
			return nil
		}
		return r.say("For the edge: the user is planning a Lisbon weekend at Hotel Aurora (rated 9.1) " +
			"with Belem on Saturday, and asked to turn on the living room lamp, which the device handles.")
	}
	switch r.result {
	case pipeline.TeamAssignToolName:
		return r.say("Plan: stay at Hotel Aurora (rated 9.1) and visit Belem on Saturday.")
	case "":
	default:
		return r.unexpected()
	}
	switch {
	case strings.Contains(r.input, "[Task t-1 from edge]"):
		if err := s.master.waitGate(ctx); err != nil {
			return nil
		}
		return r.say("The Oceanarium opens at 10:00 every day.")
	case strings.Contains(r.input, "Plan a weekend"):
		return r.call("", pipeline.TeamAssignToolName, map[string]any{"member": "researcher", "prompt": "Find the best-rated hotel in notes/hotels.md."})
	case strings.Contains(r.input, "living room lamp"):
		// The summary fork the transfer starts waits for the edge's answer,
		// so its entry lands after it.
		s.master.setGate(s.afterEdgeReply())
		return r.call("The device controls the lamp; handing over.", participant.ToolTransfer, map[string]any{
			"to": "edge", "goal": "Turn on the living room lamp",
			"done": []string{"Planned the Lisbon weekend"}, "remaining": []string{"switch the living room lamp on"},
		})
	}
	return r.unexpected()
}

// researcherTurn answers the researcher's requests.
func researcherTurn(_ context.Context, r *request) *model.ChatResponse {
	switch r.result {
	case "read_file":
		return r.say("Hotel Aurora is the best-rated hotel (9.1); Hotel Tejo has 7.4.")
	case "":
		return r.call("", "read_file", map[string]any{"path": "notes/hotels.md"})
	}
	return r.unexpected()
}

// ---------------------------------------------------------------- fake model

// request is what a scripted turn sees of one model call.
type request struct {
	m      *fakeModel
	input  string // text of the newest user message
	result string // tool name when the newest message carries a tool result
	fork   bool   // a summary fork (tool choice none)
	tokens int
}

func (r *request) say(text string) *model.ChatResponse { return r.call(text, "", nil) }

// call answers with optional text and, when name is set, one tool call.
func (r *request) call(text, name string, args map[string]any) *model.ChatResponse {
	var blocks []message.ContentBlock
	out := 0
	if text != "" {
		blocks = append(blocks, message.TextBlock{Type: "text", ID: r.m.nextID("text"), Text: text})
		out += (utf8.RuneCountInString(text) + 3) / 4
	}
	if name != "" {
		raw, _ := json.Marshal(args)
		blocks = append(blocks, message.ToolCallBlock{Type: "tool_call", ID: r.m.nextID("call"), Name: name, Input: string(raw), State: message.ToolCallPending})
		out += (len(raw) + 3) / 4
	}
	return &model.ChatResponse{Content: blocks, Usage: &model.ChatUsage{InputTokens: r.tokens, OutputTokens: out}}
}

func (r *request) unexpected() *model.ChatResponse {
	r.m.fail(fmt.Errorf("%s: unexpected request (input %q, tool result %q)", r.m.name, r.input, r.result))
	return r.say("(no scripted answer)")
}

// fakeModel is a scripted model.ChatModel: respond picks the answer from the
// request. Its usage reports are estimates of the request text, so they are
// the same on every run.
type fakeModel struct {
	name    string
	respond func(context.Context, *request) *model.ChatResponse

	mu   sync.Mutex
	ids  int
	gate <-chan struct{}
	errs []error
}

func (m *fakeModel) nextID(prefix string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ids++
	return fmt.Sprintf("%s-%s-%d", m.name, prefix, m.ids)
}

func (m *fakeModel) setGate(g <-chan struct{}) {
	m.mu.Lock()
	m.gate = g
	m.mu.Unlock()
}

// waitGate waits for the gate set last, if any.
func (m *fakeModel) waitGate(ctx context.Context) error {
	m.mu.Lock()
	g := m.gate
	m.mu.Unlock()
	if g == nil {
		return nil
	}
	select {
	case <-g:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *fakeModel) fail(err error) {
	m.mu.Lock()
	m.errs = append(m.errs, err)
	m.mu.Unlock()
}

func (m *fakeModel) problems() []error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.errs)
}

// Chat implements model.ChatModel.
func (m *fakeModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	var o model.CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	r := &request{m: m, tokens: m.CountTokens(msgs, o.Tools), fork: o.ToolChoice != nil && o.ToolChoice.Mode == "none"}
	for _, s := range o.Tools {
		if s.Function.Name == "generate_structured_output" {
			// No compaction is expected in this short session.
			return r.unexpected(), nil
		}
	}
	if n := len(msgs); n > 0 && msgs[n-1] != nil {
		for _, b := range msgs[n-1].Content {
			if tr, ok := b.(message.ToolResultBlock); ok {
				r.result = tr.Name
			}
		}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i] != nil && msgs[i].Role == message.RoleUser {
			if t := msgs[i].GetTextContent("\n"); t != nil {
				r.input = *t
				break
			}
		}
	}
	resp := m.respond(ctx, r)
	if resp == nil {
		return nil, ctx.Err()
	}
	return resp, nil
}

// ChatStream sends the text as one delta, then the final response.
func (m *fakeModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
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

// CountTokens estimates (runes+3)/4 of the message texts and tool schemas.
func (m *fakeModel) CountTokens(msgs []*message.Msg, tools []model.ToolSchema) int {
	n := 0
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		n += 4
		for _, b := range msg.Content {
			switch v := b.(type) {
			case message.TextBlock:
				n += utf8.RuneCountInString(v.Text)
			case message.ToolCallBlock:
				n += len(v.Name) + len(v.Input)
			case message.ToolResultBlock:
				n += len(v.Name) + utf8.RuneCountInString(v.GetOutputText())
			}
		}
	}
	for _, s := range tools {
		n += len(s.Function.Name) + len(s.Function.Description) + len(s.Function.Parameters)
	}
	return (n + 3) / 4
}

// ---------------------------------------------------------------- events

// watcher drains the Coordinator's events, closes the channels waiting for
// an entry and keeps error events.
type watcher struct {
	mu    sync.Mutex
	waits []*wait
	errs  []error
}

type wait struct {
	match func(*thread.Entry) bool
	ch    chan struct{}
	done  bool
}

// when returns a channel closed when an entry matching match is shown.
func (w *watcher) when(match func(*thread.Entry) bool) <-chan struct{} {
	wt := &wait{match: match, ch: make(chan struct{})}
	w.mu.Lock()
	w.waits = append(w.waits, wt)
	w.mu.Unlock()
	return wt.ch
}

func (w *watcher) run(events <-chan thread.Event) {
	for ev := range events {
		w.mu.Lock()
		switch {
		case ev.Kind == thread.EventError:
			w.errs = append(w.errs, fmt.Errorf("coordinator error event: %w", ev.Err))
		case ev.Kind == thread.EventEntry && ev.Entry != nil:
			for _, wt := range w.waits {
				if !wt.done && wt.match(ev.Entry) {
					wt.done = true
					close(wt.ch)
				}
			}
		}
		w.mu.Unlock()
	}
}

func (w *watcher) problems() []error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.errs)
}
