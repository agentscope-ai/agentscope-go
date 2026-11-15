package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
)

func callMsg(id string, extra ...message.ContentBlock) *message.Msg {
	blocks := []message.ContentBlock{message.ToolCallBlock{
		Type: "tool_call", ID: id, Name: "f", Input: "{}", State: message.ToolCallPending,
	}}
	return message.NewMsg("a", message.RoleAssistant, append(blocks, extra...))
}

func resultMsg(id string) *message.Msg {
	return message.NewMsg("a", message.RoleAssistant, []message.ContentBlock{message.ToolResultBlock{
		Type: "tool_result", ID: id, Name: "f", Output: "ok", State: message.ToolResultSuccess,
	}})
}

func textMsg(body string) *message.Msg {
	return message.NewMsg("u", message.RoleUser, body)
}

// compress_context runs from inside the acting loop, so the assistant message
// holding the batch's tool calls is already in the context. Summarizing it away
// would orphan the results that land moments later, and providers reject a tool
// result with no matching call (upstream #2143 tracks these as
// unfinished_tool_call_ids).
func TestClampForUnfinishedCallsKeepsInFlightCallsReserved(t *testing.T) {
	msgs := []*message.Msg{
		textMsg("old question"), // 0 - safe to compress
		resultMsg("done-1"),     // 1 - has no call at all, still safe
		callMsg("done-1"),       // 2 - finished pair
		textMsg("middle"),       // 3
		callMsg("in-flight"),    // 4 - NO result yet: must stay reserved
	}

	got := pullSplitBackForToolPairs(msgs, 5)
	if got != 4 {
		t.Errorf("split = %d, want 4 (the in-flight call at index 4 must not be compressed)", got)
	}

	// Once the result lands, the whole history is compressible again.
	msgs = append(msgs, resultMsg("in-flight"))
	if got := pullSplitBackForToolPairs(msgs, 6); got != 6 {
		t.Errorf("split = %d, want 6 once every call has a result", got)
	}
}

// The backward pass must also repair a call/result pair that the split
// separated, and it has to iterate: pulling one pair back can expose another.
func TestPullSplitBackForToolPairsRepairsSplitPairs(t *testing.T) {
	msgs := []*message.Msg{
		textMsg("q"),     // 0
		callMsg("c1"),    // 1 - result lands at 4
		textMsg("noise"), // 2
		callMsg("c2"),    // 3 - result lands at 5
		resultMsg("c1"),  // 4
		resultMsg("c2"),  // 5
	}

	// Split at 4 separates c1 (compressed) from its result (reserved). Pulling
	// back to 1 also drags c2 into the reserved half, which is correct: c2's
	// result is reserved too.
	if got := pullSplitBackForToolPairs(msgs, 4); got != 1 {
		t.Errorf("split = %d, want 1", got)
	}
	// A split that already keeps both pairs together must not move.
	if got := pullSplitBackForToolPairs(msgs, 2); got != 1 {
		t.Errorf("split = %d, want 1 (c1 at index 1 is compressed, its result is not)", got)
	}
	// Everything compressed is already consistent: both calls and both results
	// land in the same half, so there is nothing to pull back.
	if got := pullSplitBackForToolPairs(msgs, 6); got != 6 {
		t.Errorf("split = %d, want 6 (no pair is separated)", got)
	}
}

// A result whose call has already left the context entirely cannot be repaired
// by pulling back; that case belongs to adjustSplitForToolPairs, which runs
// first and pushes it into the compressed half.
func TestPullSplitBackIgnoresResultsWithoutAnyCall(t *testing.T) {
	msgs := []*message.Msg{
		textMsg("q"),      // 0
		resultMsg("gone"), // 1 - no matching call anywhere
		textMsg("tail"),   // 2
	}
	if got := pullSplitBackForToolPairs(msgs, 1); got != 1 {
		t.Errorf("split = %d, want 1 (nothing to pull back)", got)
	}
}

// The composition used by splitContextForCompression: forward pass first, then
// the backward pass, which must have the last word.
func TestAdjustThenPullBackDoesNotStrandInFlightCalls(t *testing.T) {
	msgs := []*message.Msg{
		textMsg("q"),         // 0
		callMsg("done"),      // 1
		resultMsg("done"),    // 2
		callMsg("in-flight"), // 3 - no result yet
		textMsg("more"),      // 4
		resultMsg("orphan"),  // 5 - call already gone; forward pass pushes past it
	}
	forward := adjustSplitForToolPairs(msgs, 4)
	if forward <= 4 {
		t.Fatalf("forward pass should have pushed past the orphan result, got %d", forward)
	}
	got := pullSplitBackForToolPairs(msgs, forward)
	if got > 3 {
		t.Errorf("split = %d; the in-flight call at index 3 was swept into the summary", got)
	}
	if got != 3 {
		t.Errorf("split = %d, want exactly 3", got)
	}
}

func TestClampForUnfinishedCallsEdgeCases(t *testing.T) {
	msgs := []*message.Msg{textMsg("a"), callMsg("x"), textMsg("b")}

	if got := pullSplitBackForToolPairs(msgs, 0); got != 0 {
		t.Errorf("split 0 must stay 0, got %d", got)
	}
	if got := pullSplitBackForToolPairs(msgs, -1); got != -1 {
		t.Errorf("a negative split must pass through, got %d", got)
	}
	if got := pullSplitBackForToolPairs(msgs, 99); got != 99 {
		t.Errorf("an out-of-range split must pass through, got %d", got)
	}
	if got := pullSplitBackForToolPairs(nil, 3); got != 3 {
		t.Errorf("nil msgs must pass the split through, got %d", got)
	}
	// Only calls inside the compressed portion matter; an in-flight call in
	// the reserved tail is already safe.
	if got := pullSplitBackForToolPairs(msgs, 1); got != 1 {
		t.Errorf("split 1 (compressing only the text msg) = %d, want 1", got)
	}
}

// The tool must never tell the model that details were summarized away when
// nothing happened, and it must be able to compress BELOW the automatic
// threshold — otherwise the model can never trigger it before the agent does.
func TestCompressContextForToolUsesLowerThresholdAndReportsHonestly(t *testing.T) {
	// ContextSize 100, TriggerRatio 0.8 -> automatic threshold 80.
	// AgentDrivenTriggerRatio unset -> 0.4 -> tool threshold 40.
	// tokenCount 60 sits between the two.
	mock := &compressionMockModel{
		tokenCount:   60,
		contextSize:  100,
		chatResponse: summaryToolCallResponse(nil),
	}
	a := NewUnifiedAgent("t", "sys", mock,
		WithContextConfig(&ContextConfig{ContextSize: 100, TriggerRatio: 0.8, ReserveRatio: 0.1}),
	)
	for i := 0; i < 6; i++ {
		a.state.Context = append(a.state.Context, textMsg("some history"))
	}

	res, err := a.compressContextForTool(context.Background())
	if err != nil {
		t.Fatalf("compressContextForTool: %v", err)
	}
	if !res.Compressed {
		t.Error("the tool-driven path must compress at its lower threshold; it reported a no-op")
	}

	// Same agent, fresh state: the automatic path must NOT compress at 60%.
	mock2 := &compressionMockModel{tokenCount: 60, contextSize: 100, chatResponse: summaryToolCallResponse(nil)}
	b := NewUnifiedAgent("t2", "sys", mock2,
		WithContextConfig(&ContextConfig{ContextSize: 100, TriggerRatio: 0.8, ReserveRatio: 0.1}),
	)
	for i := 0; i < 6; i++ {
		b.state.Context = append(b.state.Context, textMsg("some history"))
	}
	if err := b.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	if b.state.Summary != "" {
		t.Errorf("automatic compression fired at 60%% of an 80%% threshold: summary = %q", b.state.Summary)
	}
}

// Below even the agent-driven threshold the tool must report an honest no-op.
func TestCompressContextForToolReportsNoopBelowThreshold(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 5, contextSize: 100, chatResponse: summaryToolCallResponse(nil)}
	a := NewUnifiedAgent("t", "sys", mock,
		WithContextConfig(&ContextConfig{ContextSize: 100, TriggerRatio: 0.8, ReserveRatio: 0.1}),
	)
	a.state.Context = append(a.state.Context, textMsg("short"))

	res, err := a.compressContextForTool(context.Background())
	if err != nil {
		t.Fatalf("compressContextForTool: %v", err)
	}
	if res.Compressed {
		t.Error("nothing was over the threshold, so nothing may be reported as compressed")
	}
	if a.state.Summary != "" {
		t.Errorf("summary was written despite a no-op: %q", a.state.Summary)
	}
}

// With no context config at all the seam must still be safe to call and must
// say why nothing happened.
func TestCompressContextForToolWithoutConfig(t *testing.T) {
	a := NewUnifiedAgent("t", "sys", imgTestModel{})
	res, err := a.compressContextForTool(context.Background())
	if err != nil {
		t.Fatalf("compressContextForTool: %v", err)
	}
	if res.Compressed {
		t.Error("no config => nothing compressed")
	}
	if res.Detail == "" {
		t.Error("the tool needs a detail string so its reply explains the no-op")
	}
}

// The tool registered by WithAgentDrivenCompression must be wired to the
// honest seam and report the real outcome end to end.
func TestAgentDrivenCompressionToolReportsHonestly(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 5, contextSize: 100, chatResponse: summaryToolCallResponse(nil)}
	a := NewUnifiedAgent("t", "sys", mock,
		WithContextConfig(&ContextConfig{ContextSize: 100, TriggerRatio: 0.8, ReserveRatio: 0.1}),
		WithAgentDrivenCompression(),
	)
	a.state.Context = append(a.state.Context, textMsg("short"))

	resp, err := a.toolkit.CallTool(context.Background(), "compress_context", map[string]any{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.State == message.ToolResultError {
		t.Fatalf("state = %v, want success (a no-op is not a failure)", resp.State)
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("content[0] = %T, want TextBlock", resp.Content[0])
	}
	if tb.Text == "" {
		t.Error("empty tool result: the model gets no feedback at all")
	}
	if !strings.Contains(tb.Text, "No compression was needed") {
		t.Errorf("tool claimed success for a no-op: %q", tb.Text)
	}
}

// The agent-driven threshold must stay below the automatic one, and an explicit
// override above it must be clamped down rather than making the tool more eager
// than the agent.
func TestAgentDrivenTriggerRatioResolution(t *testing.T) {
	tests := []struct {
		name string
		cfg  ContextConfig
		want float64
	}{
		{"default halves the trigger", ContextConfig{TriggerRatio: 0.8}, 0.4},
		{"explicit override wins", ContextConfig{TriggerRatio: 0.8, AgentDrivenTriggerRatio: 0.6}, 0.6},
		{"override above trigger is clamped", ContextConfig{TriggerRatio: 0.5, AgentDrivenTriggerRatio: 0.9}, 0.5},
		{"unset trigger falls back to a sane value", ContextConfig{}, 0.4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentDrivenTriggerRatio(&tc.cfg); got != tc.want {
				t.Errorf("agentDrivenTriggerRatio = %v, want %v", got, tc.want)
			}
		})
	}
}

// limitContextImages replaces image blocks in place on *Msg pointers that are
// shared with snapshots handed out earlier (prepareModelInput, checkpoint
// saves, HTTP history reads). Those snapshots are read after the lock is
// released, so the rewrite has to be copy-on-write.
func TestLimitContextImagesDoesNotMutateSharedMessages(t *testing.T) {
	a := NewUnifiedAgent("t", "sys", imgTestModel{},
		WithContextConfig(&ContextConfig{MaxImageNum: 1}),
	)
	original := &message.Msg{
		Name: "u", Role: message.RoleUser,
		Content: []message.ContentBlock{imageBlock("i1", "image/png"), imageBlock("i2", "image/png")},
	}
	a.state.Context = []*message.Msg{original}

	// A snapshot taken the way prepareModelInput takes one: the same *Msg
	// pointer, read after the lock is released.
	snapshot := make([]*message.Msg, len(a.state.Context))
	copy(snapshot, a.state.Context)
	snapshotContent := snapshot[0].Content

	a.limitContextImages(1)

	// The caller's message must be untouched: same pointer, same blocks.
	if snapshotContent[0] != imageBlock("i1", "image/png") {
		t.Errorf("the shared message's first block was rewritten in place: %#v", snapshotContent[0])
	}
	if snapshotContent[1] != imageBlock("i2", "image/png") {
		t.Errorf("the shared message's second block was rewritten in place: %#v", snapshotContent[1])
	}

	// The agent's own context must have the replacement.
	a.mu.Lock()
	updated := a.state.Context[0]
	a.mu.Unlock()
	if updated == original {
		t.Error("the context still points at the shared message; the clone was not published")
	}
	replaced := 0
	for _, b := range updated.Content {
		if _, isImage := b.(message.DataBlock); !isImage {
			replaced++
		}
	}
	if replaced != 1 {
		t.Errorf("%d blocks replaced, want exactly 1 (MaxImageNum=1)", replaced)
	}
}

// A nested image inside a tool-result block list shares that list's backing
// array with the snapshot too, so the list itself has to be cloned.
func TestLimitContextImagesClonesNestedBlockLists(t *testing.T) {
	a := NewUnifiedAgent("t", "sys", imgTestModel{},
		WithContextConfig(&ContextConfig{MaxImageNum: 0}),
	)
	nested := []message.ContentBlock{
		message.TextBlock{Type: "text", Text: "[shot.png: image/png, 12 bytes]"},
		imageBlock("i1", "image/png"),
	}
	original := &message.Msg{
		Name: "a", Role: message.RoleAssistant,
		Content: []message.ContentBlock{message.ToolResultBlock{
			Type: "tool_result", ID: "tc1", Name: "Read",
			Output: nested, State: message.ToolResultSuccess,
		}},
	}
	a.state.Context = []*message.Msg{original}

	a.limitContextImages(0)

	if _, stillImage := nested[1].(message.DataBlock); !stillImage {
		t.Error("the caller's nested block list was rewritten in place")
	}
	if _, stillText := nested[0].(message.TextBlock); !stillText {
		t.Errorf("the nested text block was clobbered: %#v", nested[0])
	}

	a.mu.Lock()
	updated := a.state.Context[0]
	a.mu.Unlock()
	tr, ok := updated.Content[0].(message.ToolResultBlock)
	if !ok {
		t.Fatalf("content[0] = %T, want ToolResultBlock", updated.Content[0])
	}
	list, ok := tr.Output.([]message.ContentBlock)
	if !ok {
		t.Fatalf("Output = %T, want a block list", tr.Output)
	}
	if _, isImage := list[1].(message.DataBlock); isImage {
		t.Error("the agent's own copy still holds the image; it should be a text reminder")
	}
	// The tool result must stay readable by text-only consumers.
	if tr.GetOutputText() == "" {
		t.Error("GetOutputText is empty after the image was replaced")
	}
}

// The compression seam must survive a middleware chain: the override has to
// reach the handler that actually runs.
func TestCompressContextWithRatioHonoursOverrideUnderMiddleware(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 60, contextSize: 100, chatResponse: summaryToolCallResponse(nil)}
	mw := &ratioRecordingMiddleware{}
	a := NewUnifiedAgent("t", "sys", mock,
		WithContextConfig(&ContextConfig{ContextSize: 100, TriggerRatio: 0.8, ReserveRatio: 0.1}),
		WithMiddlewares(mw),
	)
	for i := 0; i < 6; i++ {
		a.state.Context = append(a.state.Context, textMsg("history"))
	}

	compressed, err := a.compressContextWithRatio(context.Background(), 0.4, false)
	if err != nil {
		t.Fatalf("compressContextWithRatio: %v", err)
	}
	if !compressed {
		t.Error("the override did not reach the compression handler")
	}
	seen := mw.seenRatios()
	if len(seen) != 1 || seen[0] != 0.4 {
		t.Errorf("middleware saw trigger ratios %v, want exactly [0.4]", seen)
	}
}

// ratioRecordingMiddleware is a pass-through middleware that participates in
// the compress chain and records the trigger ratio it was handed.
type ratioRecordingMiddleware struct {
	middleware.BaseMiddleware

	mu     sync.Mutex
	ratios []float64
}

func (m *ratioRecordingMiddleware) OnCompressContext(
	ctx context.Context,
	input middleware.CompressInput,
	next middleware.CompressHandler,
) error {
	m.mu.Lock()
	m.ratios = append(m.ratios, input.TriggerRatio)
	m.mu.Unlock()
	return next(ctx, input)
}

func (m *ratioRecordingMiddleware) seenRatios() []float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]float64, len(m.ratios))
	copy(out, m.ratios)
	return out
}
