package participant

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// ---------------------------------------------------------------- host

// fakeHost records every call and answers from scripted fields.
type fakeHost struct {
	mu sync.Mutex

	descs     []thread.Descriptor
	entries   []thread.Entry
	events    *thread.EventList
	readErr   error
	writeErr  error
	pending   func(call int, cursor uint64) (*thread.Sync, error)
	pendCalls int
	seq       uint64

	transfers []thread.TransferRequest
	assigns   []thread.AssignRequest
	reports   []thread.ReportRequest
	publishes []thread.PublishRequest
	reads     []thread.ReadQuery
	recorded  []thread.EventRecord
	usage     []thread.LedgerEntry
	// afterWrite runs after a write committed, before it returns, with the
	// context the write was called with.
	afterWrite func(ctx context.Context, seq uint64)
}

func newFakeHost(descs ...thread.Descriptor) *fakeHost {
	return &fakeHost{descs: descs, events: thread.NewEventList(thread.EventListOptions{}), seq: 100}
}

func (h *fakeHost) nextSeq() uint64 {
	h.seq++
	return h.seq
}

// committed runs afterWrite for a write that committed as seq.
func (h *fakeHost) committed(ctx context.Context, seq uint64) {
	h.mu.Lock()
	hook := h.afterWrite
	h.mu.Unlock()
	if hook != nil {
		hook(ctx, seq)
	}
}

func (h *fakeHost) Transfer(ctx context.Context, req thread.TransferRequest) (uint64, error) { //nolint:gocritic // the thread.Host contract
	h.mu.Lock()
	h.transfers = append(h.transfers, req)
	if h.writeErr != nil {
		defer h.mu.Unlock()
		return 0, h.writeErr
	}
	seq := h.nextSeq()
	h.mu.Unlock()
	h.committed(ctx, seq)
	return seq, nil
}

func (h *fakeHost) Assign(ctx context.Context, req thread.AssignRequest) (string, uint64, error) { //nolint:gocritic // the thread.Host contract
	h.mu.Lock()
	h.assigns = append(h.assigns, req)
	if h.writeErr != nil {
		defer h.mu.Unlock()
		return "", 0, h.writeErr
	}
	id, seq := fmt.Sprintf("task-%d", len(h.assigns)), h.nextSeq()
	h.mu.Unlock()
	h.committed(ctx, seq)
	return id, seq, nil
}

func (h *fakeHost) Report(ctx context.Context, req thread.ReportRequest) (uint64, error) { //nolint:gocritic // the thread.Host contract
	h.mu.Lock()
	h.reports = append(h.reports, req)
	if h.writeErr != nil {
		defer h.mu.Unlock()
		return 0, h.writeErr
	}
	seq := h.nextSeq()
	h.mu.Unlock()
	h.committed(ctx, seq)
	return seq, nil
}

func (h *fakeHost) Publish(ctx context.Context, req thread.PublishRequest) (uint64, error) { //nolint:gocritic // the thread.Host contract
	h.mu.Lock()
	h.publishes = append(h.publishes, req)
	if h.writeErr != nil {
		defer h.mu.Unlock()
		return 0, h.writeErr
	}
	seq := h.nextSeq()
	h.mu.Unlock()
	h.committed(ctx, seq)
	return seq, nil
}

func (h *fakeHost) zoneOf(name string) thread.Zone {
	for i := range h.descs {
		if h.descs[i].Name == name {
			return h.descs[i].Zone
		}
	}
	return thread.ZoneCloud
}

func (h *fakeHost) Read(_ context.Context, _ string, q thread.ReadQuery) ([]thread.Entry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads = append(h.reads, q)
	if h.readErr != nil {
		return nil, h.readErr
	}
	var out []thread.Entry
	for i := range h.entries {
		e := h.entries[i]
		if len(q.Seqs) > 0 {
			if !slices.Contains(q.Seqs, e.Seq) {
				continue
			}
		} else if e.Seq <= q.After {
			continue
		}
		if len(q.Kinds) > 0 && !slices.Contains(q.Kinds, e.Kind) {
			continue
		}
		out = append(out, e)
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

func (h *fakeHost) EventOutline(reader string, limit int) []thread.EventSummary {
	return h.events.Outline(h.zoneOf(reader), limit)
}

func (h *fakeHost) EventSearch(reader, query string, limit int) []thread.EventSummary {
	return h.events.Search(h.zoneOf(reader), query, limit)
}

func (h *fakeHost) EventPage(reader, id string, offset, limit int) (string, int, error) {
	return h.events.Page(h.zoneOf(reader), id, offset, limit)
}

func (h *fakeHost) RecordEvent(r thread.EventRecord) string { //nolint:gocritic // the thread.Host contract
	h.mu.Lock()
	h.recorded = append(h.recorded, r)
	h.mu.Unlock()
	return h.events.Add(r)
}

func (h *fakeHost) RecordUsage(e thread.LedgerEntry) { //nolint:gocritic // the thread.Host contract
	h.mu.Lock()
	h.usage = append(h.usage, e)
	h.mu.Unlock()
}

func (h *fakeHost) Pending(_ context.Context, _ string, cursor uint64) (*thread.Sync, error) {
	h.mu.Lock()
	h.pendCalls++
	call, fn := h.pendCalls, h.pending
	h.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	return fn(call, cursor)
}

func (h *fakeHost) Descriptors() []thread.Descriptor {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]thread.Descriptor(nil), h.descs...)
}

func (h *fakeHost) Owner() (string, uint64) { return "cloud", 1 }

func (h *fakeHost) snapshot() fakeHost {
	h.mu.Lock()
	defer h.mu.Unlock()
	return fakeHost{
		transfers: append([]thread.TransferRequest(nil), h.transfers...),
		assigns:   append([]thread.AssignRequest(nil), h.assigns...),
		reports:   append([]thread.ReportRequest(nil), h.reports...),
		publishes: append([]thread.PublishRequest(nil), h.publishes...),
		recorded:  append([]thread.EventRecord(nil), h.recorded...),
		usage:     append([]thread.LedgerEntry(nil), h.usage...),
		reads:     append([]thread.ReadQuery(nil), h.reads...),
	}
}

// ---------------------------------------------------------------- model

// scriptModel answers model calls through respond and records each call's
// rendered input text and options.
type scriptModel struct {
	mu      sync.Mutex
	inputs  []string
	opts    []model.CallOptions
	respond func(ctx context.Context, call int, o *model.CallOptions) (*model.ChatResponse, error)
}

func (m *scriptModel) record(msgs []*message.Msg, opts []model.CallOption) (int, model.CallOptions) {
	var o model.CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inputs = append(m.inputs, renderMsgs(msgs))
	m.opts = append(m.opts, o)
	return len(m.inputs) - 1, o
}

func (m *scriptModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	i, o := m.record(msgs, opts)
	return m.respond(ctx, i, &o)
}

func (m *scriptModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	resp, err := m.Chat(ctx, msgs, opts...)
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

func (m *scriptModel) CountTokens(msgs []*message.Msg, _ []model.ToolSchema) int {
	return len(renderMsgs(msgs)) / 4
}

func (m *scriptModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.inputs)
}

func (m *scriptModel) input(i int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inputs[i]
}

// steps answers call i with the i-th response, then with "extra call".
func steps(resps ...*model.ChatResponse) func(context.Context, int, *model.CallOptions) (*model.ChatResponse, error) {
	return func(_ context.Context, i int, _ *model.CallOptions) (*model.ChatResponse, error) {
		if i < len(resps) {
			return resps[i], nil
		}
		return textResp("extra call"), nil
	}
}

func renderMsgs(msgs []*message.Msg) string {
	var sb strings.Builder
	for _, m := range msgs {
		if m == nil {
			continue
		}
		for _, b := range m.Content {
			switch blk := b.(type) {
			case message.TextBlock:
				sb.WriteString(blk.Text)
			case message.ToolCallBlock:
				sb.WriteString("call " + blk.Name + " " + blk.Input)
			case message.ToolResultBlock:
				sb.WriteString("result " + blk.Name + ": " + blk.GetOutputText())
			}
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func textResp(text string) *model.ChatResponse {
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", ID: "t-" + text, Text: text}},
		Usage:   &model.ChatUsage{InputTokens: 10, OutputTokens: 3},
	}
}

func call(id, name string, args map[string]any) message.ToolCallBlock {
	raw, _ := json.Marshal(args)
	return message.ToolCallBlock{Type: "tool_call", ID: id, Name: name, Input: string(raw), State: message.ToolCallPending}
}

// resp builds a response of text (when not empty) followed by tool calls.
func resp(text string, calls ...message.ToolCallBlock) *model.ChatResponse {
	var blocks []message.ContentBlock
	if text != "" {
		blocks = append(blocks, message.TextBlock{Type: "text", ID: "t-" + text, Text: text})
	}
	for _, c := range calls {
		blocks = append(blocks, c)
	}
	return &model.ChatResponse{Content: blocks, Usage: &model.ChatUsage{InputTokens: 20, OutputTokens: 5, CacheInputTokens: 4}}
}

// ---------------------------------------------------------------- tools and agents

var objectSchema = json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"url":{"type":"string"}}}`)

func textTool(name, output string) tool.Tool {
	return tool.NewFunctionTool(name, "test tool "+name, objectSchema, func(context.Context, map[string]any) (any, error) {
		return output, nil
	})
}

var (
	cloudDesc = thread.Descriptor{Name: "cloud", Zone: thread.ZoneCloud, Description: "cloud agent", Window: 200000}
	edgeDesc  = thread.Descriptor{Name: "edge", Zone: thread.ZoneEdge, Description: "edge agent", Window: 8000, BriefMaxRunes: 2000}
)

type fixture struct {
	p     *AgentParticipant
	a     *agent.UnifiedAgent
	m     *scriptModel
	host  *fakeHost
	perms *permission.Context
}

type fixtureOptions struct {
	desc     thread.Descriptor
	prepare  PrepareOptions
	cfg      AgentConfig
	tools    []tool.Tool
	agent    []agent.AgentOption
	respond  func(context.Context, int, *model.CallOptions) (*model.ChatResponse, error)
	mode     permission.PermissionMode
	noBind   bool
	noTools  bool
	noPrep   bool
	noOption bool
}

func newFixture(t *testing.T, o fixtureOptions) *fixture { //nolint:gocritic // test helper
	t.Helper()
	if o.desc.Name == "" {
		o.desc = cloudDesc
	}
	if o.mode == "" {
		o.mode = permission.ModeBypass
	}
	f := &fixture{m: &scriptModel{respond: o.respond}, host: newFakeHost(cloudDesc, edgeDesc), perms: permission.NewContext(o.mode)}
	cfg := o.cfg
	cfg.Descriptor = o.desc
	optIntoEvents(o.desc, &o.prepare, &cfg.CaptureOutputs)
	if o.noPrep {
		tk := tool.NewToolkit(o.tools...)
		opts := append([]agent.AgentOption{agent.WithToolkit(tk), agent.WithModelStreaming(false), agent.WithPermissionContext(f.perms)}, o.agent...)
		f.a = agent.NewUnifiedAgent(o.desc.Name, "sys", f.m, opts...)
		cfg.Agent = f.a
		p, err := NewAgentParticipant(cfg)
		if err != nil {
			t.Fatal(err)
		}
		f.p = p
	} else {
		prep, err := Prepare(o.desc, o.prepare)
		if err != nil {
			t.Fatal(err)
		}
		all := o.tools
		if !o.noTools {
			all = append(prep.Tools(), o.tools...)
		}
		opts := append([]agent.AgentOption{agent.WithToolkit(tool.NewToolkit(all...)), agent.WithModelStreaming(false), agent.WithPermissionContext(f.perms)}, o.agent...)
		if !o.noOption {
			opts = append(opts, prep.Options()...)
		}
		f.a = agent.NewUnifiedAgent(o.desc.Name, "sys", f.m, opts...)
		cfg.Agent = f.a
		p, err := prep.Participant(cfg)
		if err != nil {
			t.Fatal(err)
		}
		f.p = p
	}
	if !o.noBind {
		if err := f.p.Bind(f.host); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// trigger returns a shared user_input entry.
func trigger(seq uint64, text string) thread.Entry {
	return thread.Entry{Seq: seq, Kind: thread.KindUserInput,
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: text}}}
}

func foreground(seq uint64, input string) thread.Activation {
	return thread.Activation{Epoch: 3, Foreground: true, Trigger: trigger(seq, input), Input: input}
}

// drain reads an activation to its end. onEvent may answer confirmations.
// It fails unless exactly one End item arrives, last, and the channel closes.
func drain(t *testing.T, ch <-chan thread.ActivationEvent, onEvent func(thread.ActivationEvent)) ([]thread.ActivationEvent, *thread.ActivationEnd) {
	t.Helper()
	var items []thread.ActivationEvent
	var end *thread.ActivationEnd
	timeout := time.After(10 * time.Second)
	for {
		select {
		case it, ok := <-ch:
			if !ok {
				if end == nil {
					t.Fatal("the channel closed without an End item")
				}
				return items, end
			}
			if end != nil {
				t.Fatalf("an item arrived after End: %+v", it)
			}
			if it.End != nil {
				if it.Event != nil {
					t.Fatal("an End item also carries an event")
				}
				end = it.End
				continue
			}
			if it.Event == nil {
				t.Fatal("an item has neither event nor End")
			}
			items = append(items, it)
			if onEvent != nil {
				onEvent(it)
			}
		case <-timeout:
			t.Fatal("the activation did not end")
		}
	}
}

func activate(t *testing.T, p thread.Participant, act thread.Activation, onEvent func(thread.ActivationEvent)) ([]thread.ActivationEvent, *thread.ActivationEnd) { //nolint:gocritic // test helper
	t.Helper()
	ch, err := p.Activate(context.Background(), act)
	if err != nil {
		t.Fatal(err)
	}
	return drain(t, ch, onEvent)
}

// confirmAll answers every confirmation request of participant p.
func confirmAll(t *testing.T, p thread.Participant, asks *int) func(thread.ActivationEvent) {
	return func(it thread.ActivationEvent) {
		req, ok := it.Event.(event.RequireUserConfirmEvent)
		if !ok {
			return
		}
		*asks++
		results := make([]event.ConfirmResult, len(req.ToolCalls))
		for i, c := range req.ToolCalls {
			results[i] = event.ConfirmResult{Confirmed: true, ToolCall: c}
		}
		if err := p.SubmitUserConfirm(it.Path, &event.UserConfirmResultEvent{ReplyID: req.ReplyID, ConfirmResults: results}); err != nil {
			t.Errorf("submit confirmation: %v", err)
		}
	}
}

func eventsOf[T event.Event](items []thread.ActivationEvent) []T {
	var out []T
	for _, it := range items {
		if e, ok := it.Event.(T); ok {
			out = append(out, e)
		}
	}
	return out
}

func entryText(seq uint64, kind thread.Kind, text string) thread.Entry {
	return thread.Entry{Seq: seq, Kind: kind, Origin: thread.Origin{Participant: "edge"},
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: text}}}
}

func projected(seq uint64, text string) *message.Msg {
	m := message.UserMsg("thread", text)
	m.Metadata[thread.SeqMetadataKey] = seq
	m.Metadata[thread.KindMetadataKey] = string(thread.KindReport)
	return m
}

// optIntoEvents opts a test participant into the event list the way the
// defaults did before it became opt-in: every thread tool (publish_artifact
// only for the cloud) and capture on the cloud. Explicit settings win.
func optIntoEvents(d thread.Descriptor, p *PrepareOptions, capture **bool) { //nolint:gocritic // test helper
	if p.Tools == nil {
		for _, n := range allToolNames {
			if n == ToolPublishArtifact && d.Zone == thread.ZoneEdge {
				continue
			}
			p.Tools = append(p.Tools, n)
		}
	}
	if *capture == nil {
		on := d.Zone == thread.ZoneCloud
		*capture = &on
	}
}
