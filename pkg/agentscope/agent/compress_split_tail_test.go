package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// These tests cover a split whose last message holds a tool result while the
// call is in an earlier message. The forward repair used to move such a split
// past every message, so compression kept none.

// splitTailModel answers compression calls (recognized by the default
// compression prompt) with a structured summary and every other call from a
// script, and records what each call was sent. A message costs 10 tokens plus
// a quarter of its text, call-input and result bytes.
type splitTailModel struct {
	mu               sync.Mutex
	script           []model.ChatResponse
	mainInputs       [][]string
	compressInputs   [][]string
	compressionErr   error
	compressionCalls int
	// beforeMain, when set, runs before each non-compression call, outside
	// the model's lock (the background compression call needs it).
	beforeMain func()
	// beforeCompression, when set, runs before each compression call,
	// outside the model's lock.
	beforeCompression func()
}

func (m *splitTailModel) Chat(_ context.Context, msgs []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	compression := isSplitTailCompression(msgs)
	rendered := renderSplitTail(msgs)
	if !compression && m.beforeMain != nil {
		m.beforeMain()
	}
	if compression && m.beforeCompression != nil {
		m.beforeCompression()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if compression {
		m.compressionCalls++
		m.compressInputs = append(m.compressInputs, rendered)
		if m.compressionErr != nil {
			return nil, m.compressionErr
		}
		return summaryToolCallResponse(nil), nil
	}
	m.mainInputs = append(m.mainInputs, rendered)
	if len(m.script) == 0 {
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true}, nil
	}
	resp := m.script[0]
	m.script = m.script[1:]
	return &resp, nil
}

func (m *splitTailModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (m *splitTailModel) CountTokens(msgs []*message.Msg, _ []model.ToolSchema) int {
	total := 0
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		n := 0
		for _, b := range msg.Content {
			switch blk := b.(type) {
			case message.TextBlock:
				n += len(blk.Text)
			case message.ToolCallBlock:
				n += len(blk.Input)
			case message.ToolResultBlock:
				n += len(blk.GetOutputText())
			}
		}
		total += 10 + n/4
	}
	return total
}

func isSplitTailCompression(msgs []*message.Msg) bool {
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		for _, b := range msg.Content {
			if tb, ok := b.(message.TextBlock); ok && strings.Contains(tb.Text, "Now write a continuation summary") {
				return true
			}
		}
	}
	return false
}

// renderSplitTail renders one line per message: role, then text("<first
// 40 bytes>"), call(<id>) or result(<id>) per block.
func renderSplitTail(msgs []*message.Msg) []string {
	out := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		if msg == nil {
			out = append(out, "<nil>")
			continue
		}
		parts := []string{string(msg.Role) + ":"}
		for _, b := range msg.Content {
			switch blk := b.(type) {
			case message.TextBlock:
				parts = append(parts, fmt.Sprintf("text(%q)", blk.Text[:min(len(blk.Text), 40)]))
			case message.ToolCallBlock:
				parts = append(parts, "call("+blk.ID+")")
			case message.ToolResultBlock:
				parts = append(parts, "result("+blk.ID+")")
			}
		}
		out = append(out, strings.Join(parts, " "))
	}
	return out
}

// splitTailHistory is [user, assistant(text, call x), user, assistant(result
// x)]: the last message holds the result of a call made two messages
// earlier. With the system prompt "helpful" (11 tokens) the messages cost
// 10+(13+firstBytes)/4, 110, 13 and 210 tokens.
func splitTailHistory(firstBytes int) []*message.Msg {
	return []*message.Msg{
		message.UserMsg("user", "start the job"+strings.Repeat("s", firstBytes)),
		message.AssistantMsg("bot", []message.ContentBlock{
			message.TextBlock{Type: "text", Text: strings.Repeat("w", 400)},
			message.ToolCallBlock{Type: "tool_call", ID: "x", Name: "f", Input: `{}`, State: message.ToolCallFinished},
		}),
		message.UserMsg("user", "use the result"),
		message.AssistantMsg("bot", []message.ContentBlock{
			message.ToolResultBlock{Type: "tool_result", ID: "x", Name: "f", Output: strings.Repeat("r", 800), State: message.ToolResultSuccess},
		}),
	}
}

func TestSplitKeepsLastMessageWhoseCallIsEarlier(t *testing.T) {
	withoutCall := splitTailHistory(0)
	withoutCall[1] = message.AssistantMsg("bot", strings.Repeat("w", 400))
	resultBetween := splitTailHistory(0)
	resultBetween[2] = message.AssistantMsg("bot", []message.ContentBlock{
		message.ToolResultBlock{Type: "tool_result", ID: "gone", Name: "f", Output: "old", State: message.ToolResultSuccess},
	})
	callFirst := splitTailHistory(0)[1:]
	roundAfter := append(splitTailHistory(0), message.AssistantMsg("bot", strings.Repeat("t", 100)))
	call := func(id string) message.ContentBlock {
		return message.ToolCallBlock{Type: "tool_call", ID: id, Name: "f", Input: `{}`, State: message.ToolCallFinished}
	}
	result := func(id, out string) message.ContentBlock {
		return message.ToolResultBlock{Type: "tool_result", ID: id, Name: "f", Output: out, State: message.ToolResultSuccess}
	}
	// r is answered in the second message, answered again in the fourth and
	// reused by a call still in flight in the last; y spans the last two.
	reusedID := []*message.Msg{
		message.UserMsg("user", "start the job"),
		message.AssistantMsg("bot", []message.ContentBlock{call("r"), result("r", "first")}),
		message.UserMsg("user", "use the result"),
		message.AssistantMsg("bot", []message.ContentBlock{call("y"), result("r", strings.Repeat("r", 800))}),
		message.AssistantMsg("bot", []message.ContentBlock{call("r"), result("y", "ok")}),
	}
	// r is answered in the second message; the fourth reuses it and is
	// answered there, and the last holds another result of r.
	answeredTwice := []*message.Msg{
		message.UserMsg("user", "start the job"),
		message.AssistantMsg("bot", []message.ContentBlock{call("r"), result("r", "first")}),
		message.UserMsg("user", "use the result"),
		message.AssistantMsg("bot", []message.ContentBlock{call("y"), call("r"), result("r", strings.Repeat("r", 800))}),
		message.AssistantMsg("bot", []message.ContentBlock{result("r", "again"), result("y", "ok")}),
	}

	tests := []struct {
		name        string
		msgs        []*message.Msg
		budget      int
		freshWindow bool
		wantKept    int
	}{
		// The newest two messages (234 tokens with the system prompt) fit the
		// 300-token reserve; the call's message is taken in with them.
		{name: "call two messages earlier", msgs: splitTailHistory(0), budget: 300, wantKept: 3},
		// The messages after the call fit (269 tokens with the system
		// prompt); its message is taken in with them.
		{name: "call before the messages that fit", msgs: roundAfter, budget: 300, wantKept: 4},
		// The kept messages may exceed the reserve, as for a call in flight.
		{name: "kept messages exceed the reserve", msgs: splitTailHistory(0), budget: 150, wantKept: 3},
		// The forward push stays the repair for a result whose call is gone.
		{name: "call not in the context", msgs: withoutCall, budget: 300, wantKept: 0},
		{name: "kept range holds a result whose call is gone", msgs: resultBetween, budget: 300, wantKept: 0},
		// Taking in y's call would keep the second result of r ahead of any
		// call of r among the kept messages; the later call reusing r does
		// not pair it.
		{name: "a later call reuses the result's ID", msgs: reusedID, budget: 100, wantKept: 0},
		// Taking in y's call would keep two results of r and one call of r.
		{name: "a call answers one result", msgs: answeredTwice, budget: 100, wantKept: 0},
		// Keeping the pair would leave nothing to compress.
		{name: "call in the first message", msgs: callFirst, budget: 300, wantKept: 0},
		// A fresh window folds every finished exchange into the summary.
		{name: "fresh window", msgs: splitTailHistory(0), budget: 1, freshWindow: true, wantKept: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewUnifiedAgent("bot", "helpful", &splitTailModel{})
			a.state.Context = tt.msgs
			compressed, kept := a.splitContextForCompression(tt.budget, tt.freshWindow, nil)
			if len(kept) != tt.wantKept {
				t.Fatalf("kept %d messages, want %d\nkept: %v", len(kept), tt.wantKept, renderSplitTail(kept))
			}
			if len(compressed)+len(kept) != len(tt.msgs) {
				t.Fatalf("compressed %d + kept %d != %d messages", len(compressed), len(kept), len(tt.msgs))
			}
			for i, m := range kept {
				if m != tt.msgs[len(compressed)+i] {
					t.Fatalf("kept message %d is not the stored message", i)
				}
			}
			if id := orphanToolResult(kept); id != "" {
				t.Fatalf("kept messages hold result %q without its call", id)
			}
		})
	}
}

// contentSnapshot copies each message's blocks so a later comparison shows
// whether a stored message was changed in place.
func contentSnapshot(msgs []*message.Msg) [][]message.ContentBlock {
	out := make([][]message.ContentBlock, len(msgs))
	for i, m := range msgs {
		out[i] = append([]message.ContentBlock(nil), m.Content...)
	}
	return out
}

// checkKeptTail checks that ctx is the stored tail, unchanged, and that every
// result in it has its call.
func checkKeptTail(t *testing.T, ctx, tail []*message.Msg, before [][]message.ContentBlock) {
	t.Helper()
	if len(ctx) != len(tail) {
		t.Fatalf("context has %d messages, want the %d newest: %v", len(ctx), len(tail), renderSplitTail(ctx))
	}
	for i := range tail {
		if ctx[i] != tail[i] || !reflect.DeepEqual(ctx[i].Content, before[i]) {
			t.Fatalf("kept message %d is not the stored message, unchanged", i)
		}
	}
	if id := orphanToolResult(ctx); id != "" {
		t.Fatalf("context holds result %q without its call", id)
	}
}

func TestCompactKeepsResultWhoseCallIsEarlier(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprintf("strict=%v", strict), func(t *testing.T) {
			m := &splitTailModel{}
			history := splitTailHistory(0)
			a := NewUnifiedAgent("bot", "helpful", m,
				WithContextConfig(&ContextConfig{ContextSize: 1000, ReserveRatio: 0.3}))
			a.state.Context = append([]*message.Msg(nil), history...)
			before := contentSnapshot(history[1:])

			var err error
			if strict {
				_, err = a.CompactStrict(context.Background(), CompactStrictOptions{})
			} else {
				_, err = a.Compact(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
			checkKeptTail(t, a.state.Context, history[1:], before)
			if a.state.Summary == "" {
				t.Fatal("no summary installed")
			}
			if got := strings.Join(m.compressInputs[0], "\n"); !strings.Contains(got, "start the job") || strings.Contains(got, "call(x)") {
				t.Fatalf("compression request should hold only the first message:\n%s", got)
			}
		})
	}
}

const splitTailInput = "continue with the external result"

// newResumedExternalAgent restores a reply parked on an external call, as a
// host restores it after a restart: [user, assistant(text, call tc_x)]. A
// reply with new input appends the input after the call's message, asks for
// the call again (repromptAwaitingCalls) and records its result in a new
// message after the input, because saveToContext merges only into a trailing
// assistant message of this agent: [user, assistant(text, call tc_x),
// user(input), assistant(result tc_x)]. The reply's later rounds join the
// result's message. With the system prompt "helpful" the restored messages
// cost 10+(13+firstBytes)/4 and 110 tokens; the input costs 18 and an
// 800-byte result 210.
func newResumedExternalAgent(m *splitTailModel, cfg *ContextConfig, firstBytes int, extra ...tool.Tool) *UnifiedAgent {
	restored := &AgentState{
		SchemaVersion: StateSchemaVersion,
		SessionID:     "sess-split-tail",
		Context: []*message.Msg{
			message.UserMsg("user", "start the job"+strings.Repeat("s", firstBytes)),
			message.AssistantMsg("cp-agent", []message.ContentBlock{
				message.TextBlock{Type: "text", Text: strings.Repeat("w", 400)},
				message.ToolCallBlock{Type: "tool_call", ID: "tc_x", Name: "ext_tool", Input: `{}`, State: message.ToolCallSubmitted},
			}),
		},
	}
	permCtx := permission.NewContext(permission.ModeDefault)
	for _, name := range []string{"ext_tool", "echo_big"} {
		permCtx.AllowRules[name] = []permission.Rule{{ToolName: name, Behavior: permission.BehaviorAllow}}
	}
	tools := append([]tool.Tool{newHITLMockTool("ext_tool", true)}, extra...)
	return NewUnifiedAgent("cp-agent", "helpful", m,
		WithToolkit(tool.NewToolkit(tools...)),
		WithPermissionContext(permCtx),
		WithReactConfig(ReactConfig{MaxIters: 6}),
		WithContextConfig(cfg),
		WithState(restored),
	)
}

// runResumedReply runs a reply with splitTailInput, answers the external call
// it asks for again with an 800-byte result, and drains the reply.
func runResumedReply(t *testing.T, a *UnifiedAgent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ch, err := a.ReplyStream(ctx, splitTailInput)
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	for evt := range ch {
		switch e := evt.(type) {
		case event.RequireExternalExecutionEvent:
			asked++
			results := make([]message.ToolResultBlock, 0, len(e.ToolCalls))
			for _, tc := range e.ToolCalls {
				results = append(results, message.ToolResultBlock{
					Type: "tool_result", ID: tc.ID, Name: tc.Name,
					Output: strings.Repeat("r", 800), State: message.ToolResultSuccess,
				})
			}
			a.SubmitExternalResult(&event.ExternalExecutionResultEvent{ReplyID: e.ReplyID, ExecutionResults: results})
		case event.RequireUserConfirmEvent:
			t.Errorf("the reply asked for a confirmation")
			cancel()
		}
	}
	if ctx.Err() != nil {
		t.Fatal("the reply did not finish")
	}
	if asked != 1 {
		t.Fatalf("the external call was asked for %d times, want once", asked)
	}
}

// echoBigTool returns a 1000-byte result.
func echoBigTool() tool.Tool {
	return tool.NewFunctionTool("echo_big", "echo", nil, func(context.Context, map[string]any) (any, error) {
		return strings.Repeat("y", 1000), nil
	})
}

// echoRounds scripts one model round per ID, each calling echo_big. Added
// with its result to the message holding tc_x's result, a round adds 23 + 2 +
// 1000 bytes to it, which makes that message 256 tokens larger here.
func echoRounds(ids ...string) []model.ChatResponse {
	rounds := make([]model.ChatResponse, 0, len(ids))
	for _, id := range ids {
		rounds = append(rounds, model.ChatResponse{Content: []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "checking one more thing"},
			message.ToolCallBlock{Type: "tool_call", ID: id, Name: "echo_big", Input: `{}`, State: message.ToolCallPending},
		}, IsLast: true})
	}
	return rounds
}

// The automatic trigger on the step after tc_x's result: 11 + 463 + 110 + 18
// + 210 = 812 tokens, at the 800-token trigger. The newest two messages fit
// the 300-token reserve and the call's message is taken in with them, so only
// the first message is summarized, also when summary generation fails (the
// truncation fallback installs the same kept messages). The kept messages are
// the stored ones, unchanged.
func TestCompressionKeepsResultAfterInput(t *testing.T) {
	for _, summaryFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("summaryFails=%v", summaryFails), func(t *testing.T) {
			m := &splitTailModel{}
			if summaryFails {
				m.compressionErr = errors.New("summary model unavailable")
			}
			var a *UnifiedAgent
			var stored, kept []*message.Msg
			var storedContent, keptContent [][]message.ContentBlock
			readContext := func() []*message.Msg {
				a.mu.Lock()
				defer a.mu.Unlock()
				return append([]*message.Msg(nil), a.state.Context...)
			}
			m.beforeCompression = func() {
				if stored == nil {
					stored = readContext()
					storedContent = contentSnapshot(stored)
				}
			}
			m.beforeMain = func() {
				if kept == nil {
					kept = readContext()
					keptContent = contentSnapshot(kept)
				}
			}
			a = newResumedExternalAgent(m, &ContextConfig{ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.3}, 1800)
			runResumedReply(t, a)

			// What the next model call is sent from: the stored messages
			// from the call's on, unchanged (the reply adds its answer to the
			// last one afterwards).
			if len(stored) != 4 || len(kept) != 3 {
				t.Fatalf("context has %d messages before compression and %d after, want 4 and 3", len(stored), len(kept))
			}
			for i := range kept {
				if kept[i] != stored[i+1] || !reflect.DeepEqual(keptContent[i], storedContent[i+1]) {
					t.Fatalf("kept message %d is not the stored message, unchanged", i)
				}
			}

			m.mu.Lock()
			defer m.mu.Unlock()
			if m.compressionCalls < 1 {
				t.Fatal("compression did not run")
			}
			if got := strings.Join(m.compressInputs[0], "\n"); strings.Contains(got, "call(tc_x)") || strings.Contains(got, splitTailInput) {
				t.Fatalf("compression request should hold only the first message:\n%s", got)
			}
			if len(m.mainInputs) != 1 {
				t.Fatalf("main calls = %d, want 1", len(m.mainInputs))
			}
			next := strings.Join(m.mainInputs[0], "\n")
			for _, want := range []string{"call(tc_x)", splitTailInput, "result(tc_x)"} {
				if !strings.Contains(next, want) {
					t.Errorf("next model request lacks %s:\n%s", want, next)
				}
			}
			if id := orphanToolResult(a.state.Context); id != "" {
				t.Fatalf("context holds result %q without its call", id)
			}
		})
	}
}

// Background precompression in the same reply. The snapshot is taken at the
// step after tc_x's result (562 tokens, past the 500-token arm point), with
// the result's message last; the call's
// message is taken in with the newest two, so the result keeps three
// messages. The next round, tc_y and its result, joins the result's message
// (818 tokens, at the trigger). The trigger installs the kept messages' live
// versions, so tc_y reaches the next request.
func TestBackgroundPrecompressKeepsResultAfterInput(t *testing.T) {
	m := &splitTailModel{script: echoRounds("tc_y")}
	var a *UnifiedAgent
	var ready *precompressResult
	wait := waitForBackground(t, &a, &ready)
	var stored, kept []*message.Msg
	var storedContent, keptContent [][]message.ContentBlock
	m.beforeMain = func() {
		wait()
		a.mu.Lock()
		ctx := append([]*message.Msg(nil), a.state.Context...)
		a.mu.Unlock()
		if stored == nil {
			stored, storedContent = ctx, contentSnapshot(ctx)
		} else if kept == nil {
			kept, keptContent = ctx, contentSnapshot(ctx)
		}
	}
	a = newResumedExternalAgent(m, &ContextConfig{
		ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.3, PrecompressRatio: 0.5,
	}, 800, echoBigTool())
	runResumedReply(t, a)

	// The trigger installs the stored messages from the call's on: the
	// call's message and the input unchanged, the result's message grown only
	// by the round added after the snapshot (its text, tc_y and tc_y's
	// result).
	if len(stored) != 4 || len(kept) != 3 {
		t.Fatalf("context has %d messages at the first model call and %d after the trigger, want 4 and 3", len(stored), len(kept))
	}
	for i := range kept {
		if kept[i] != stored[i+1] {
			t.Fatalf("kept message %d is not the stored message", i)
		}
	}
	for i := 0; i < 2; i++ {
		if !reflect.DeepEqual(keptContent[i], storedContent[i+1]) {
			t.Fatalf("kept message %d was changed", i)
		}
	}
	n := len(storedContent[3])
	if len(keptContent[2]) != n+3 || !reflect.DeepEqual(keptContent[2][:n], storedContent[3]) {
		t.Fatalf("the result's message is %v, want its stored blocks and the round", keptContent[2])
	}
	if c, ok := keptContent[2][n+1].(message.ToolCallBlock); !ok || c.ID != "tc_y" {
		t.Fatalf("the round's call is %v, want tc_y", keptContent[2][n+1])
	}
	if r, ok := keptContent[2][n+2].(message.ToolResultBlock); !ok || r.ID != "tc_y" {
		t.Fatalf("the round's result is %v, want tc_y's", keptContent[2][n+2])
	}

	if ready == nil {
		t.Fatal("no background result was ready before the first model call")
	}
	if ready.snapshotLen != 4 || ready.reservedLen != 3 {
		t.Fatalf("background result snapshotLen=%d reservedLen=%d, want 4 and 3", ready.snapshotLen, ready.reservedLen)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.compressionCalls != 1 {
		t.Fatalf("compression calls = %d, want 1 (the background one)", m.compressionCalls)
	}
	if len(m.mainInputs) != 2 {
		t.Fatalf("main calls = %d, want 2", len(m.mainInputs))
	}
	next := strings.Join(m.mainInputs[1], "\n")
	for _, want := range []string{"call(tc_x)", splitTailInput, "result(tc_x)", "call(tc_y)", "result(tc_y)"} {
		if !strings.Contains(next, want) {
			t.Errorf("request after the trigger lacks %s:\n%s", want, next)
		}
	}
}

// With a fresh window the background split in the same reply keeps no
// message: the forward repair pushes the result's message into the summary
// with its call. Such a result would be installed as the messages after the
// snapshot, so the next round, which joins the result's message, would be
// dropped unsummarized. No result is made (runPrecompress), and the trigger
// summarizes the live context, tc_y included.
func TestBackgroundPrecompressFreshWindowKeepsRoundJoiningResult(t *testing.T) {
	m := &splitTailModel{script: echoRounds("tc_y")}
	var a *UnifiedAgent
	var ready *precompressResult
	m.beforeMain = waitForBackground(t, &a, &ready)
	a = newResumedExternalAgent(m, &ContextConfig{
		ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.3, PrecompressRatio: 0.5,
		CompactStrategy: CompactStrategyFreshWindow,
	}, 800, echoBigTool())
	runResumedReply(t, a)

	if ready != nil {
		t.Fatalf("background result made with reservedLen=%d", ready.reservedLen)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.compressInputs) != 1 || len(m.mainInputs) != 2 {
		t.Fatalf("compression calls = %d, main calls = %d; want 1 (the trigger's) and 2", len(m.compressInputs), len(m.mainInputs))
	}
	summarized := strings.Join(m.compressInputs[0], "\n")
	for _, want := range []string{splitTailInput, "result(tc_x)", "call(tc_y)", "result(tc_y)"} {
		if !strings.Contains(summarized, want) {
			t.Errorf("the trigger's compression request lacks %s:\n%s", want, summarized)
		}
	}
}

// waitForBackground returns a beforeMain hook that waits for a running
// background generation to finish before the model answers, so the split runs
// on the snapshot before the reply adds the next round, and records the
// result it made.
func waitForBackground(t *testing.T, a **UnifiedAgent, ready **precompressResult) func() {
	return func() {
		deadline := time.Now().Add(10 * time.Second)
		for {
			(*a).mu.Lock()
			p, running := (*a).pendingPrecompress, (*a).precompressRunning
			(*a).mu.Unlock()
			if !running {
				if p != nil {
					*ready = p
				}
				return
			}
			if time.Now().After(deadline) {
				t.Error("background generation did not finish")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// One round after tc_x's result: the round joins the result's message (466
// tokens). The
// messages after the call fit the reserve and the call's message does not:
// 11 + 18 + 466 = 495 tokens, 605 with it, against a 550-token reserve (the
// step is at 905, the trigger at 800). The call's message is taken in, so the
// input stays in the context, also when summary generation fails.
func TestCompressionAfterRoundKeepsInputBeforeResult(t *testing.T) {
	for _, summaryFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("summaryFails=%v", summaryFails), func(t *testing.T) {
			m := &splitTailModel{script: echoRounds("tc_y")}
			if summaryFails {
				m.compressionErr = errors.New("summary model unavailable")
			}
			a := newResumedExternalAgent(m, &ContextConfig{ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.55}, 1147, echoBigTool())
			runResumedReply(t, a)

			m.mu.Lock()
			defer m.mu.Unlock()
			if m.compressionCalls < 1 || len(m.mainInputs) != 2 {
				t.Fatalf("compression calls = %d, main calls = %d; want at least 1 and 2", m.compressionCalls, len(m.mainInputs))
			}
			if got := strings.Join(m.compressInputs[0], "\n"); strings.Contains(got, splitTailInput) {
				t.Fatalf("compression request should hold only the first message:\n%s", got)
			}
			next := strings.Join(m.mainInputs[1], "\n")
			for _, want := range []string{"call(tc_x)", splitTailInput, "result(tc_x)", "call(tc_y)", "result(tc_y)"} {
				if !strings.Contains(next, want) {
					t.Errorf("request after the compaction lacks %s:\n%s", want, next)
				}
			}
			if id := orphanToolResult(a.state.Context); id != "" {
				t.Fatalf("context holds result %q without its call", id)
			}
		})
	}
}

// The background guard on its own, run synchronously: a split that keeps no
// message (a fresh window here) makes no result and sends no model call,
// while the default strategy keeps the snapshot's last three messages.
func TestRunPrecompressSkipsSplitKeepingNothing(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("freshWindow=%v", fresh), func(t *testing.T) {
			m := &splitTailModel{}
			cfg := ContextConfig{ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.3, PrecompressRatio: 0.5}
			if fresh {
				cfg.CompactStrategy = CompactStrategyFreshWindow
			}
			a := NewUnifiedAgent("bot", "helpful", m, WithContextConfig(&cfg))
			snapshot := splitTailHistory(0)
			a.state.Context = append([]*message.Msg(nil), snapshot...)
			norm := cfg.withDefaults()
			a.runPrecompress(context.Background(), &norm, 1000, snapshot, contextDigest(snapshot), "", "helpful", nil, nil)

			a.mu.Lock()
			p := a.pendingPrecompress
			a.mu.Unlock()
			if fresh {
				if p != nil || m.compressionCalls != 0 {
					t.Fatalf("result made (%v) or model called (%d times) for a split keeping no message", p != nil, m.compressionCalls)
				}
				return
			}
			if p == nil || p.reservedLen != 3 || m.compressionCalls != 1 {
				t.Fatalf("want one model call and a result keeping 3 messages; calls=%d result=%+v", m.compressionCalls, p)
			}
		})
	}
}
