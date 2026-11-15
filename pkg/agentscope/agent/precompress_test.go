package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// precompressMockModel counts tokens per message so the estimate grows with
// the context, and serves structured-summary responses to compression calls.
type precompressMockModel struct {
	contextSize      int
	perMessageTokens int64
	chatCalls        atomic.Int64
	chatErr          error
}

func (m *precompressMockModel) Chat(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	m.chatCalls.Add(1)
	if m.chatErr != nil {
		return nil, m.chatErr
	}
	return summaryToolCallResponse(&model.ChatUsage{InputTokens: 11, OutputTokens: 7}), nil
}

func (m *precompressMockModel) ChatStream(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (m *precompressMockModel) CountTokens(msgs []*message.Msg, _ []model.ToolSchema) int {
	return int(atomic.LoadInt64(&m.perMessageTokens)) * len(msgs)
}

func (m *precompressMockModel) ContextSize() int { return m.contextSize }

func (m *precompressMockModel) ModelName() string { return "precompress-mock" }

func newPrecompressAgent(mock *precompressMockModel, cfg *ContextConfig) *UnifiedAgent {
	return NewUnifiedAgent("t", "sys", mock, WithContextConfig(cfg))
}

func seedContext(a *UnifiedAgent, n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := 0; i < n; i++ {
		a.state.Context = append(a.state.Context, message.NewMsg("u", message.RoleUser, "message body"))
	}
}

// waitPrecompress polls until the background generation settles: either a
// pending result is installed, or the run finished without one (failure or
// nothing to compress).
func waitPrecompress(t *testing.T, a *UnifiedAgent) *precompressResult {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		p, running := a.pendingPrecompress, a.precompressRunning
		a.mu.Unlock()
		if !running {
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background pre-compression did not settle")
	return nil
}

// TestPrecompress_ArmsUnderThreshold: between the precompress ratio and the
// hard trigger, compressContext kicks off background generation without
// touching the live state.
func TestPrecompress_ArmsUnderThreshold(t *testing.T) {
	mock := &precompressMockModel{contextSize: 1000, perMessageTokens: 100}
	a := newPrecompressAgent(mock, &ContextConfig{
		ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.3, PrecompressRatio: 0.5,
	})
	// system + 4 context messages = 500 tokens: past the 500 arm line, under
	// the 800 hard trigger.
	seedContext(a, 4)

	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	p := waitPrecompress(t, a)
	if p == nil {
		t.Fatal("expected a pending pre-compression result")
	}
	if got := mock.chatCalls.Load(); got != 1 {
		t.Errorf("chat calls = %d, want 1 (the background generation)", got)
	}
	a.mu.Lock()
	summary, ctxLen := a.state.Summary, len(a.state.Context)
	a.mu.Unlock()
	if summary != "" || ctxLen != 4 {
		t.Errorf("arming must not mutate state: summary=%q ctxLen=%d", summary, ctxLen)
	}
	// The generation burned tokens; usage is recorded for the reply loop to
	// drain.
	u := a.takeCompressionUsage()
	if u == nil || u.InputTokens != 11 || u.OutputTokens != 7 {
		t.Errorf("compression usage = %+v, want in:11 out:7", u)
	}
}

// TestPrecompress_AppliesAtHardTrigger: with a valid result pending, the hard
// trigger installs it without a new model call; messages appended after the
// snapshot survive raw.
func TestPrecompress_AppliesAtHardTrigger(t *testing.T) {
	mock := &precompressMockModel{contextSize: 1000, perMessageTokens: 100}
	a := newPrecompressAgent(mock, &ContextConfig{
		ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.3, PrecompressRatio: 0.5,
	})
	seedContext(a, 4)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	if waitPrecompress(t, a) == nil {
		t.Fatal("expected a pending pre-compression result")
	}
	a.mu.Lock()
	boundary := a.state.Context[3]
	a.mu.Unlock()

	// Conversation continues past the hard trigger: 8 context messages +
	// system = 900 tokens >= 800.
	seedContext(a, 4)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext at trigger: %v", err)
	}

	if got := mock.chatCalls.Load(); got != 1 {
		t.Errorf("chat calls = %d, want 1: the ready result must apply without regenerating", got)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingPrecompress != nil {
		t.Error("pending result must be consumed by the apply")
	}
	if !strings.Contains(a.state.Summary, "T") {
		t.Errorf("summary = %q, want the generated summary", a.state.Summary)
	}
	// Reserve budget 300 fits one message next to the system prompt, so the
	// snapshot reserved its last message; the four appended afterwards stay.
	if len(a.state.Context) != 5 {
		t.Fatalf("context len = %d, want 5 (1 reserved + 4 appended)", len(a.state.Context))
	}
	if a.state.Context[0] != boundary {
		t.Error("reserved message must be the live boundary message, not a snapshot copy")
	}
}

// TestPrecompress_StillOverAfterApplyCompresses: messages appended since the
// snapshot can keep the context at the threshold after the ready summary is
// applied; the trigger then also runs the synchronous compression instead of
// letting the reply send an over-threshold request.
func TestPrecompress_StillOverAfterApplyCompresses(t *testing.T) {
	mock := &precompressMockModel{contextSize: 1000, perMessageTokens: 100}
	a := newPrecompressAgent(mock, &ContextConfig{
		ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.3, PrecompressRatio: 0.5,
	})
	seedContext(a, 4)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	if waitPrecompress(t, a) == nil {
		t.Fatal("expected a pending pre-compression result")
	}

	// Twelve context messages. The ready summary replaces three of them, which
	// leaves system + summary + 9 messages = 1100 tokens, still >= 800.
	seedContext(a, 8)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext at trigger: %v", err)
	}
	if got := mock.chatCalls.Load(); got != 2 {
		t.Fatalf("chat calls = %d, want 2: the synchronous compression must follow the applied result", got)
	}
	if est := mock.CountTokens(a.prepareModelInput(context.Background()), nil); est >= 800 {
		t.Errorf("estimate after compression = %d, want < 800", est)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingPrecompress != nil {
		t.Error("pending result must be consumed by the apply")
	}
	if len(a.state.Context) >= 9 {
		t.Fatalf("context len = %d: the context was not compressed past the ready result", len(a.state.Context))
	}
}

// TestPrecompress_CompactAppliesReadyResultOnly: Compact asks for one
// compaction, so a ready result satisfies it even when the context stays
// over the threshold; only the automatic trigger compresses further.
func TestPrecompress_CompactAppliesReadyResultOnly(t *testing.T) {
	mock := &precompressMockModel{contextSize: 1000, perMessageTokens: 100}
	a := newPrecompressAgent(mock, &ContextConfig{
		ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.3, PrecompressRatio: 0.5,
	})
	seedContext(a, 4)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	if waitPrecompress(t, a) == nil {
		t.Fatal("expected a pending pre-compression result")
	}
	seedContext(a, 8)
	compressed, err := a.Compact(context.Background())
	if err != nil || !compressed {
		t.Fatalf("Compact = %v, %v; want true, nil", compressed, err)
	}
	if got := mock.chatCalls.Load(); got != 1 {
		t.Errorf("chat calls = %d, want 1: Compact applies the ready result without another call", got)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.state.Context) != 9 {
		t.Errorf("context len = %d, want 9 (1 reserved + 8 appended)", len(a.state.Context))
	}
}

// TestPrecompress_ToolTriggerCompressesPastReadyResult: the compress_context
// tool compresses at its own lower threshold, and it too compresses further
// when the ready result leaves the context at that threshold.
func TestPrecompress_ToolTriggerCompressesPastReadyResult(t *testing.T) {
	mock := &precompressMockModel{contextSize: 1000, perMessageTokens: 100}
	// Automatic trigger 800, tool threshold 400 (TriggerRatio/2), arm line 300.
	a := newPrecompressAgent(mock, &ContextConfig{
		ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.1, PrecompressRatio: 0.3,
	})
	seedContext(a, 2)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	if waitPrecompress(t, a) == nil {
		t.Fatal("expected a pending pre-compression result")
	}

	// Five context messages; the ready summary replaces one, which leaves
	// system + summary + 4 messages = 600 tokens, still >= 400.
	seedContext(a, 3)
	res, err := a.compressContextForTool(context.Background())
	if err != nil || !res.Compressed {
		t.Fatalf("compressContextForTool = %+v, %v; want compressed", res, err)
	}
	if got := mock.chatCalls.Load(); got != 2 {
		t.Fatalf("chat calls = %d, want 2: the synchronous compression must follow the applied result", got)
	}
	if est := mock.CountTokens(a.prepareModelInput(context.Background()), nil); est >= 400 {
		t.Errorf("estimate after compression = %d, want < 400", est)
	}
}

// TestPrecompress_StillOverWithEmptyContextStops: when the applied result
// leaves no message to summarize, the trigger stops after the apply instead
// of running the synchronous compression over an empty context.
func TestPrecompress_StillOverWithEmptyContextStops(t *testing.T) {
	mock := &precompressMockModel{contextSize: 1000, perMessageTokens: 100}
	// Threshold 150: system + summary alone (200) stay over it.
	a := newPrecompressAgent(mock, &ContextConfig{ContextSize: 1000, TriggerRatio: 0.15, ReserveRatio: 0.1})
	seedContext(a, 2)
	a.mu.Lock()
	last := a.state.Context[1]
	a.pendingPrecompress = &precompressResult{summary: "covers everything", snapshotLen: 2, lastMsg: last, lastMsgID: last.ID,
		prefix: contextDigest(a.state.Context[:2])}
	a.mu.Unlock()

	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	if got := mock.chatCalls.Load(); got != 0 {
		t.Errorf("chat calls = %d, want 0: nothing is left to summarize", got)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.Summary != "covers everything" || len(a.state.Context) != 0 {
		t.Errorf("summary %q with %d messages, want the ready result applied", a.state.Summary, len(a.state.Context))
	}
}

// blankFirstSummaryModel answers the first compression call with blank
// summary fields and every later call with the regular summary.
type blankFirstSummaryModel struct {
	precompressMockModel
}

func (m *blankFirstSummaryModel) Chat(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	resp := summaryToolCallResponse(nil)
	if m.chatCalls.Add(1) == 1 {
		resp.Content = []message.ContentBlock{message.ToolCallBlock{
			Type:  "tool_call",
			ID:    "tc_summary",
			Name:  "generate_structured_output",
			Input: `{"task_overview":"  "}`,
			State: message.ToolCallPending,
		}}
	}
	return resp, nil
}

// TestPrecompress_BlankSummaryDiscarded: a background generation that formats
// to a blank summary (a custom template over empty fields) is discarded like
// a failed one, so it cannot replace the history with nothing; the trigger
// then compresses synchronously.
func TestPrecompress_BlankSummaryDiscarded(t *testing.T) {
	mock := &blankFirstSummaryModel{precompressMockModel{contextSize: 1000, perMessageTokens: 100}}
	a := NewUnifiedAgent("t", "sys", mock, WithContextConfig(&ContextConfig{
		ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.3, PrecompressRatio: 0.5,
		SummaryTemplate: "{task_overview}",
	}))
	seedContext(a, 4)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	if p := waitPrecompress(t, a); p != nil {
		t.Fatalf("a blank background summary must leave no pending result, got %q", p.summary)
	}

	seedContext(a, 4) // 8 context + system = 900 >= 800
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext at trigger: %v", err)
	}
	if got := mock.chatCalls.Load(); got != 2 {
		t.Errorf("chat calls = %d, want 2: the trigger must compress synchronously", got)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.Summary != "T" {
		t.Errorf("summary = %q, want the synchronous summary", a.state.Summary)
	}
}

func TestPrecompress_BelowRatioDoesNotArm(t *testing.T) {
	mock := &precompressMockModel{contextSize: 1000, perMessageTokens: 100}
	a := newPrecompressAgent(mock, &ContextConfig{
		ContextSize: 1000, TriggerRatio: 0.8, PrecompressRatio: 0.5,
	})
	seedContext(a, 2) // 300 tokens total: below the 500 arm line

	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	a.mu.Lock()
	running, pending := a.precompressRunning, a.pendingPrecompress
	a.mu.Unlock()
	if running || pending != nil {
		t.Errorf("must not arm below the ratio: running=%v pending=%v", running, pending)
	}
	if got := mock.chatCalls.Load(); got != 0 {
		t.Errorf("chat calls = %d, want 0", got)
	}
}

func TestPrecompress_DisabledByDefault(t *testing.T) {
	mock := &precompressMockModel{contextSize: 1000, perMessageTokens: 100}
	a := newPrecompressAgent(mock, &ContextConfig{
		ContextSize: 1000, TriggerRatio: 0.8, // PrecompressRatio unset
	})
	seedContext(a, 4)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	a.mu.Lock()
	running := a.precompressRunning
	a.mu.Unlock()
	if running {
		t.Error("precompression must be opt-in")
	}
	if got := mock.chatCalls.Load(); got != 0 {
		t.Errorf("chat calls = %d, want 0", got)
	}
}

func TestPrecompress_RatioClampedWhenNotBelowTrigger(t *testing.T) {
	cfg := (&ContextConfig{TriggerRatio: 0.8, PrecompressRatio: 0.85}).withDefaults()
	if cfg.PrecompressRatio != 0 {
		t.Errorf("PrecompressRatio >= TriggerRatio must disable: got %v", cfg.PrecompressRatio)
	}
	cfg = (&ContextConfig{PrecompressRatio: -1}).withDefaults()
	if cfg.PrecompressRatio != 0 {
		t.Errorf("negative PrecompressRatio must normalize to 0, got %v", cfg.PrecompressRatio)
	}
	cfg = (&ContextConfig{TriggerRatio: 0.8, PrecompressRatio: 0.5}).withDefaults()
	if cfg.PrecompressRatio != 0.5 {
		t.Errorf("valid PrecompressRatio changed: %v", cfg.PrecompressRatio)
	}
}

// TestPrecompress_StaleResultDiscarded: a result whose base summary no longer
// matches (another compaction landed meanwhile) is discarded, and the hard
// trigger falls back to synchronous compression.
func TestPrecompress_StaleResultDiscarded(t *testing.T) {
	mock := &precompressMockModel{contextSize: 1000, perMessageTokens: 100}
	a := newPrecompressAgent(mock, &ContextConfig{
		ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.3, PrecompressRatio: 0.5,
	})
	seedContext(a, 4)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	if waitPrecompress(t, a) == nil {
		t.Fatal("expected a pending pre-compression result")
	}

	// Simulate an interleaved compaction (checkpoint rollback has the same
	// shape): the base summary the generation built upon is gone.
	a.mu.Lock()
	a.state.Summary = "someone else compacted"
	a.mu.Unlock()

	seedContext(a, 4) // 8 context + system = 900 >= 800
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext at trigger: %v", err)
	}
	if got := mock.chatCalls.Load(); got != 2 {
		t.Errorf("chat calls = %d, want 2: stale result must fall back to a synchronous call", got)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingPrecompress != nil {
		t.Error("stale result must be discarded")
	}
	if !strings.Contains(a.state.Summary, "T") {
		t.Errorf("summary = %q, want the synchronously generated one", a.state.Summary)
	}
}

// TestPrecompress_GenerationFailureDiscards: a failed background generation
// leaves no pending result and does not wedge the agent — the synchronous
// path still compresses at the hard trigger.
func TestPrecompress_GenerationFailureDiscards(t *testing.T) {
	mock := &precompressMockModel{contextSize: 1000, perMessageTokens: 100, chatErr: errors.New("boom")}
	a := newPrecompressAgent(mock, &ContextConfig{
		ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.3, PrecompressRatio: 0.5,
	})
	seedContext(a, 4)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	if p := waitPrecompress(t, a); p != nil {
		t.Fatalf("failed generation must leave no pending result, got %+v", p)
	}
	if got := mock.chatCalls.Load(); got != 1 {
		t.Errorf("chat calls = %d, want 1", got)
	}

	mock.chatErr = nil
	seedContext(a, 4)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("synchronous fallback: %v", err)
	}
	a.mu.Lock()
	summary := a.state.Summary
	a.mu.Unlock()
	if !strings.Contains(summary, "T") {
		t.Errorf("summary = %q, want the synchronous summary after background failure", summary)
	}
}

// TestPrecompress_NotArmedWithContentBlockCompaction: the block-level
// splitter has different granularity, so precompression stays off under it.
func TestPrecompress_NotArmedWithContentBlockCompaction(t *testing.T) {
	mock := &precompressMockModel{contextSize: 1000, perMessageTokens: 100}
	a := NewUnifiedAgent("t", "sys", mock,
		WithContextConfig(&ContextConfig{
			ContextSize: 1000, TriggerRatio: 0.8, PrecompressRatio: 0.5,
		}),
		WithContentBlockCompaction(),
	)
	seedContext(a, 4)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	a.mu.Lock()
	running := a.precompressRunning
	a.mu.Unlock()
	if running {
		t.Error("precompression must not arm under content-block compaction")
	}
}

// TestEmitTurnStats_DrainsCompressionUsage: compression usage that lands
// after the last Reason-state drain (e.g. a background pre-compression
// finishing during the final model call) is accounted to the ending reply:
// a model-call-end event first, then turn_stats carrying the tokens.
func TestEmitTurnStats_DrainsCompressionUsage(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 1, contextSize: 100}
	a := NewUnifiedAgent("t", "sys", mock)
	a.beginTurnStats()
	a.recordCompressionUsage(&model.ChatUsage{InputTokens: 11, OutputTokens: 7})

	ch := make(chan event.Event, 4)
	a.emitTurnStats(context.Background(), ch, "rid")
	close(ch)

	var kinds []event.EventType
	var stats *event.CustomEvent
	var callEnd *event.ModelCallEndEvent
	for evt := range ch {
		kinds = append(kinds, evt.GetEventType())
		switch e := evt.(type) {
		case event.CustomEvent:
			if e.Name == "turn_stats" {
				stats = &e
			}
		case event.ModelCallEndEvent:
			callEnd = &e
		}
	}
	if len(kinds) != 2 || kinds[0] != event.EventModelCallEnd || kinds[1] != event.EventCustom {
		t.Fatalf("event sequence = %v, want [model_call_end custom]", kinds)
	}
	if callEnd == nil || callEnd.InputTokens != 11 || callEnd.OutputTokens != 7 {
		t.Errorf("model call end = %+v, want in:11 out:7", callEnd)
	}
	if stats == nil {
		t.Fatal("missing turn_stats")
	}
	if stats.Value["model_calls"] != 1 || stats.Value["input_tokens"] != 11 || stats.Value["output_tokens"] != 7 {
		t.Errorf("turn_stats = %+v, want the drained usage included", stats.Value)
	}
	if a.takeCompressionUsage() != nil {
		t.Error("drain must clear the accumulator")
	}
}

// TestPrecompress_ResultDroppedWhenContextShrinks: a checkpoint rollback that
// leaves fewer messages than the snapshot held invalidates the ready result,
// and the compaction runs synchronously instead.
func TestPrecompress_ResultDroppedWhenContextShrinks(t *testing.T) {
	mock := &precompressMockModel{contextSize: 1000, perMessageTokens: 100}
	a := newPrecompressAgent(mock, &ContextConfig{
		ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.3, PrecompressRatio: 0.5,
	})
	seedContext(a, 4)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	if waitPrecompress(t, a) == nil {
		t.Fatal("expected a pending pre-compression result")
	}

	// Simulate a rollback to a checkpoint that held two of the four messages.
	a.mu.Lock()
	a.state.Context = a.state.Context[:2]
	a.mu.Unlock()

	compressed, err := a.Compact(context.Background())
	if err != nil || !compressed {
		t.Fatalf("Compact = %v, %v; want true, nil", compressed, err)
	}
	if got := mock.chatCalls.Load(); got != 2 {
		t.Errorf("chat calls = %d, want 2: the stale result must fall back to a synchronous call", got)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingPrecompress != nil {
		t.Error("stale result must be discarded")
	}
	if len(a.state.Context) != 1 || !strings.Contains(a.state.Summary, "T") {
		t.Errorf("summary %q with %d messages, want the synchronous compaction", a.state.Summary, len(a.state.Context))
	}
}

// TestPrecompress_CorruptOfferIsDropped: an offer read from a checkpoint the
// agent did not write must not index outside the context or install a blank
// summary. Each case passes the boundary and digest checks, so only the bounds
// and text checks stand between it and a panic or an emptied context.
func TestPrecompress_CorruptOfferIsDropped(t *testing.T) {
	cases := map[string]func(boundary, digest string) *SummaryOffer{
		"zero length": func(string, string) *SummaryOffer { return &SummaryOffer{Text: "x", Len: 0} },
		"negative keep": func(b, d string) *SummaryOffer {
			return &SummaryOffer{Text: "x", Len: 2, Keep: -5, Boundary: b, Digest: d}
		},
		"keep beyond len": func(b, d string) *SummaryOffer {
			return &SummaryOffer{Text: "x", Len: 2, Keep: 5, Boundary: b, Digest: d}
		},
		"blank summary": func(b, d string) *SummaryOffer {
			return &SummaryOffer{Text: "  ", Len: 2, Keep: 1, Boundary: b, Digest: d}
		},
		"length past state": func(b, d string) *SummaryOffer {
			return &SummaryOffer{Text: "x", Len: 9, Keep: 1, Boundary: b, Digest: d}
		},
		// OfferSummary always keeps the boundary message.
		"keep zero": func(b, d string) *SummaryOffer {
			return &SummaryOffer{Text: "x", Len: 2, Keep: 0, Boundary: b, Digest: d}
		},
	}
	for name, offer := range cases {
		t.Run(name, func(t *testing.T) {
			ctxMsgs := []*message.Msg{
				message.NewMsg("u", message.RoleUser, "one"),
				message.NewMsg("u", message.RoleUser, "two"),
				message.NewMsg("u", message.RoleUser, "three"),
			}
			st := &AgentState{Context: append([]*message.Msg(nil), ctxMsgs...),
				SummaryOffer: offer(ctxMsgs[1].ID, contextDigest(ctxMsgs[:2]))}
			mock := &precompressMockModel{contextSize: 1000, perMessageTokens: 100}
			a := NewUnifiedAgent("t", "sys", mock, WithState(st))
			if a.applyReadyPrecompress(context.Background()) {
				t.Fatal("a corrupt offer was applied")
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.state.SummaryOffer != nil || a.pendingPrecompress != nil {
				t.Fatal("a corrupt offer must be dropped")
			}
			if a.state.Summary != "" || len(a.state.Context) != 3 {
				t.Fatalf("state changed: summary %q, %d messages", a.state.Summary, len(a.state.Context))
			}
		})
	}
}

// TestPrecompress_OfferAtNilBoundaryIsDropped: a loaded offer carries no
// message pointer, so a nil message at its boundary must not match it.
func TestPrecompress_OfferAtNilBoundaryIsDropped(t *testing.T) {
	st := &AgentState{
		Context:      []*message.Msg{message.NewMsg("u", message.RoleUser, "one"), nil, message.NewMsg("u", message.RoleUser, "three")},
		SummaryOffer: &SummaryOffer{Text: "x", Len: 2, Keep: 1},
	}
	a := NewUnifiedAgent("t", "sys", &precompressMockModel{contextSize: 1000, perMessageTokens: 100}, WithState(st))
	if a.applyReadyPrecompress(context.Background()) {
		t.Fatal("an offer at a nil boundary message was applied")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.SummaryOffer != nil || a.state.Summary != "" || len(a.state.Context) != 3 {
		t.Fatalf("offer %+v, summary %q, %d messages", a.state.SummaryOffer, a.state.Summary, len(a.state.Context))
	}
}

// TestPrecompress_CorruptOfferFallsBackToSyncCompression: at the trigger a
// corrupt offer is dropped and the synchronous compression runs instead.
func TestPrecompress_CorruptOfferFallsBackToSyncCompression(t *testing.T) {
	mock := &precompressMockModel{contextSize: 1000, perMessageTokens: 100}
	st := &AgentState{SummaryOffer: &SummaryOffer{Text: "x", Len: 0}}
	a := NewUnifiedAgent("t", "sys", mock, WithState(st),
		WithContextConfig(&ContextConfig{ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.3}))
	seedContext(a, 9)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	if got := mock.chatCalls.Load(); got != 1 {
		t.Fatalf("chat calls = %d, want 1: the synchronous compression must run", got)
	}
}

// gatedPrecompressModel blocks the background generation until released.
type gatedPrecompressModel struct {
	precompressMockModel
	entered chan struct{}
	gate    chan struct{}
	once    atomic.Bool
}

func (m *gatedPrecompressModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	if m.once.CompareAndSwap(false, true) {
		close(m.entered)
	}
	<-m.gate
	return m.precompressMockModel.Chat(ctx, msgs, opts...)
}

// TestPrecompress_NewerOfferWinsOverBackgroundResult: an offer made while a
// background generation runs is newer than that generation's snapshot, so
// the finished generation must not replace it.
func TestPrecompress_NewerOfferWinsOverBackgroundResult(t *testing.T) {
	m := &gatedPrecompressModel{entered: make(chan struct{}), gate: make(chan struct{})}
	m.contextSize = 1000
	m.perMessageTokens = 100
	a := NewUnifiedAgent("t", "sys", m, WithContextConfig(&ContextConfig{
		ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.3, PrecompressRatio: 0.5,
	}))
	seedContext(a, 4)
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	select {
	case <-m.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the background generation did not start")
	}
	a.mu.Lock()
	a.state.SummaryOffer = &SummaryOffer{Text: "the offer", Len: 4, Keep: 1, Boundary: a.state.Context[3].ID,
		Digest: contextDigest(a.state.Context[:4])}
	a.pendingPrecompress = a.state.SummaryOffer.result()
	a.mu.Unlock()
	close(m.gate)

	p := waitPrecompress(t, a)
	if p == nil || p.summary != "the offer" {
		t.Fatalf("pending = %+v, want the offer", p)
	}
	if !a.applyReadyPrecompress(context.Background()) {
		t.Fatal("the offer was not applied")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.Summary != "the offer" || a.state.SummaryOffer != nil {
		t.Fatalf("summary %q, offer %+v", a.state.Summary, a.state.SummaryOffer)
	}
}
