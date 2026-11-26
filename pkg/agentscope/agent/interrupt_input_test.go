package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// A reply with new input closes the calls an earlier reply left without a
// result (a restart, or a new message instead of an answer): nothing runs and
// nothing is asked again. The result goes into the call's message, before the
// input, and says whether the call ran: a call awaiting approval did not, any
// other may have.

const interruptInput = "something else instead"

// interruptFixture is an agent restored with [user, assistant(call)], the call
// in the given state, and the tools such a call may name. ran counts local
// executions.
func interruptFixture(t *testing.T, state message.ToolCallState, opts ...AgentOption) (*UnifiedAgent, *splitTailModel, *atomic.Int32, message.ToolCallBlock) {
	t.Helper()
	ran := &atomic.Int32{}
	lookup := tool.NewFunctionTool("lookup", "lookup", nil, func(context.Context, map[string]any) (any, error) {
		ran.Add(1)
		return "looked up", nil
	})
	name := "lookup"
	if state == message.ToolCallSubmitted {
		name = "question"
	}
	call := message.ToolCallBlock{Type: "tool_call", ID: "tc_1", Name: name, Input: `{"q":1}`, State: state}
	m := &splitTailModel{}
	a := NewUnifiedAgent("bot", "helpful", m, append([]AgentOption{
		WithToolkit(tool.NewToolkit(lookup, &validatedExternalTool{BaseTool: tool.BaseTool{ToolName: "question"}})),
		WithPermissionContext(permission.NewContext(permission.ModeDefault)),
		WithReactConfig(ReactConfig{MaxIters: 3}),
		WithState(&AgentState{SchemaVersion: StateSchemaVersion, SessionID: "sess-interrupt", Context: []*message.Msg{
			message.UserMsg("user", "do the thing"),
			message.AssistantMsg("bot", []message.ContentBlock{call}),
		}}),
	}, opts...)...)
	return a, m, ran, call
}

// replyCollecting runs a reply with interruptInput and returns its events. It
// fails the test when the reply asks for anything.
func replyCollecting(t *testing.T, a *UnifiedAgent) []event.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := a.ReplyStream(ctx, interruptInput)
	if err != nil {
		t.Fatal(err)
	}
	var events []event.Event
	for ev := range ch {
		events = append(events, ev)
		switch ev.(type) {
		case event.RequireUserConfirmEvent, event.RequireExternalExecutionEvent:
			t.Errorf("the reply asked again: %T", ev)
			cancel()
		}
	}
	if ctx.Err() != nil {
		t.Fatal("the reply did not finish")
	}
	return events
}

func TestNewInputInterruptsUnansweredCalls(t *testing.T) {
	for _, tc := range []struct {
		state message.ToolCallState
		text  string
	}{
		{message.ToolCallAsking, interruptedApprovalResult},
		{message.ToolCallSubmitted, interruptedExternalResult},
		{message.ToolCallPending, interruptedUnknownResult},
		{message.ToolCallAllowed, interruptedUnknownResult},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			// Reply recovery checkpoints the state right after the input.
			saver := newFakeStateSaver()
			a, m, ran, call := interruptFixture(t, tc.state, WithStateSaver(saver), WithReplyRecovery())
			events := replyCollecting(t, a)
			if ran.Load() != 0 {
				t.Fatalf("the interrupted call ran %d times", ran.Load())
			}

			// State: the result follows the call in its message, then the input.
			got := renderSplitTail(a.state.Context)
			want := []string{`user: text("do the thing")`, "assistant: call(tc_1) result(tc_1)", fmt.Sprintf("user: text(%q)", interruptInput), `assistant: text("done")`}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("context = %q, want %q", got, want)
			}
			blocks := a.state.Context[1].Content
			if c := blocks[0].(message.ToolCallBlock); c.State != message.ToolCallFinished {
				t.Errorf("call state %s, want finished", c.State)
			}
			r := blocks[1].(message.ToolResultBlock)
			if r.State != message.ToolResultInterrupted || r.GetOutputText() != tc.text || r.Name != call.Name {
				t.Errorf("result %+v, want interrupted %q", r, tc.text)
			}

			// The model got the same layout.
			m.mu.Lock()
			sent := m.mainInputs
			m.mu.Unlock()
			if len(sent) != 1 || !reflect.DeepEqual(sent[0][len(sent[0])-3:], want[:3]) {
				t.Fatalf("model input %q, want it to end with %q", sent, want[:3])
			}

			// Events: one ToolCallsInterruptedEvent under this reply, before
			// its model call, and no tool events (the call did not run here).
			var replyID string
			var seq []string
			for _, ev := range events {
				switch e := ev.(type) {
				case event.ReplyStartEvent:
					replyID = e.ReplyID
				case event.CustomEvent:
					if e.Name != ToolCallsInterruptedEvent {
						continue
					}
					want := map[string]any{"calls": []map[string]any{{
						"tool_call_id": "tc_1", "name": call.Name, "input": call.Input, "state": string(tc.state), "result": tc.text,
					}}}
					if e.ReplyID != replyID || !reflect.DeepEqual(e.Value, want) {
						t.Errorf("interrupted event %+v, want reply %q and value %v", e, replyID, want)
					}
					seq = append(seq, "interrupted")
				case event.ModelCallStartEvent:
					seq = append(seq, "model")
				case event.ToolCallStartEvent, event.ToolResultStartEvent, event.ToolResultEndEvent:
					seq = append(seq, fmt.Sprintf("tool event %T", e))
				}
			}
			if wantSeq := []string{"interrupted", "model"}; !reflect.DeepEqual(seq, wantSeq) {
				t.Fatalf("events %q, want %q", seq, wantSeq)
			}

			// The checkpoint holds the result.
			st, err := saver.LoadState(context.Background(), "sess-interrupt")
			if err != nil {
				t.Fatal(err)
			}
			if _, found := findResultIn(st.Context, "tc_1"); !found {
				t.Fatal("the checkpoint lacks the interrupted call's result")
			}
		})
	}
}

func findResultIn(msgs []*message.Msg, id string) (message.ToolResultBlock, bool) {
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		for _, b := range msg.Content {
			if r, ok := b.(message.ToolResultBlock); ok && r.ID == id {
				return r, true
			}
		}
	}
	return message.ToolResultBlock{}, false
}

// Only this agent's calls without a result are closed. A result answers one
// call, the newest earlier one of its ID still without a result: a call
// answered in a later message keeps its result, and when an earlier message
// holds an unanswered call whose ID a later message's call and result reuse,
// the earlier call is closed. Another agent's message is left as it is.
func TestInterruptClosesOnlyUnansweredCallsOfThisAgent(t *testing.T) {
	call := func(id string, state message.ToolCallState) message.ToolCallBlock {
		return message.ToolCallBlock{Type: "tool_call", ID: id, Name: "lookup", Input: `{}`, State: state}
	}
	result := func(id string) message.ToolResultBlock {
		return message.ToolResultBlock{Type: "tool_result", ID: id, Name: "lookup", Output: "ok", State: message.ToolResultSuccess}
	}
	first := message.AssistantMsg("bot", []message.ContentBlock{
		call("dup", message.ToolCallFinished), result("dup"), call("dup", message.ToolCallAsking),
		call("late", message.ToolCallFinished),
	})
	peer := message.AssistantMsg("peer", []message.ContentBlock{call("p1", message.ToolCallAsking)})
	later := message.AssistantMsg("bot", []message.ContentBlock{result("late"), call("open", message.ToolCallPending)})
	twinOpen := message.AssistantMsg("bot", []message.ContentBlock{call("twin", message.ToolCallPending)})
	twinDone := message.AssistantMsg("bot", []message.ContentBlock{call("twin", message.ToolCallFinished), result("twin")})
	restored := []*message.Msg{message.UserMsg("user", "go"), first, message.UserMsg("user", "and"), later, twinOpen, twinDone, peer}

	a := NewUnifiedAgent("bot", "helpful", &splitTailModel{},
		WithToolkit(tool.NewToolkit(tool.NewFunctionTool("lookup", "lookup", nil, func(context.Context, map[string]any) (any, error) {
			t.Error("a call ran")
			return "", nil
		}))),
		WithPermissionContext(permission.NewContext(permission.ModeDefault)),
		WithState(&AgentState{SchemaVersion: StateSchemaVersion, SessionID: "s", Context: restored}))
	replyCollecting(t, a)

	got := renderSplitTail(a.state.Context)
	want := []string{
		`user: text("go")`,
		"assistant: call(dup) result(dup) call(dup) call(late) result(dup)",
		`user: text("and")`,
		"assistant: result(late) call(open) result(open)",
		"assistant: call(twin) result(twin)",
		"assistant: call(twin) result(twin)",
		"assistant: call(p1)",
		fmt.Sprintf("user: text(%q)", interruptInput),
		`assistant: text("done")`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("context = %q\nwant      %q", got, want)
	}
	if r := a.state.Context[1].Content[4].(message.ToolResultBlock); r.GetOutputText() != interruptedApprovalResult {
		t.Errorf("second dup result %q", r.GetOutputText())
	}
	for _, i := range []int{3, 4} {
		blocks := a.state.Context[i].Content
		if r := blocks[len(blocks)-1].(message.ToolResultBlock); r.GetOutputText() != interruptedUnknownResult {
			t.Errorf("message %d: result %q", i, r.GetOutputText())
		}
	}
	if r := a.state.Context[5].Content[1].(message.ToolResultBlock); r.GetOutputText() != "ok" {
		t.Errorf("the later twin's own result was replaced: %q", r.GetOutputText())
	}
}

// At the HTTP boundary of the real adapters, the interrupted calls' results
// directly follow the assistant message that made the calls, and the new
// input comes after them. Anthropic requires tool_result blocks first in the
// user turn right after the tool_use turn; OpenAI Chat gets one tool message
// per call before the user message.
func TestInterruptedCallsWireLayout(t *testing.T) {
	restored := func() *AgentState {
		return &AgentState{SchemaVersion: StateSchemaVersion, SessionID: "s", Context: []*message.Msg{
			message.UserMsg("user", "do the thing"),
			message.AssistantMsg("bot", []message.ContentBlock{
				message.ToolCallBlock{Type: "tool_call", ID: "call_ask", Name: "lookup", Input: `{}`, State: message.ToolCallAsking},
				message.ToolCallBlock{Type: "tool_call", ID: "call_sub", Name: "question", Input: `{}`, State: message.ToolCallSubmitted},
			}),
		}}
	}
	run := func(t *testing.T, cm model.ChatModel, stream bool) {
		lookup := tool.NewFunctionTool("lookup", "lookup", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) {
			t.Error("the interrupted call ran")
			return "", nil
		})
		a := NewUnifiedAgent("bot", "helpful", cm,
			WithToolkit(tool.NewToolkit(lookup, &validatedExternalTool{BaseTool: tool.BaseTool{ToolName: "question"}})),
			WithPermissionContext(permission.NewContext(permission.ModeDefault)),
			WithModelStreaming(stream),
			WithState(restored()))
		replyCollecting(t, a)
	}

	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("openai/stream=%v", stream), func(t *testing.T) {
			var bodies [][]byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				bodies = append(bodies, body)
				if strings.Contains(string(body), `"stream":true`) {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
			}))
			defer srv.Close()
			cm, err := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "test", Model: "test", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			run(t, cm, stream)
			if len(bodies) != 1 {
				t.Fatalf("requests = %d, want 1", len(bodies))
			}
			var req struct {
				Messages []struct {
					Role       string          `json:"role"`
					Content    json.RawMessage `json:"content"`
					ToolCallID string          `json:"tool_call_id"`
					ToolCalls  []struct {
						ID string `json:"id"`
					} `json:"tool_calls"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(bodies[0], &req); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, msg := range req.Messages {
				line := msg.Role
				for _, c := range msg.ToolCalls {
					line += " call:" + c.ID
				}
				if msg.ToolCallID != "" {
					line += " result:" + msg.ToolCallID
					if !strings.Contains(string(msg.Content), map[string]string{"call_ask": "did not run", "call_sub": "outcome is unknown"}[msg.ToolCallID]) {
						t.Errorf("result for %s: %s", msg.ToolCallID, msg.Content)
					}
				}
				if msg.Role == "user" && strings.Contains(string(msg.Content), interruptInput) {
					line += " input"
				}
				got = append(got, line)
			}
			want := []string{"system", "user", "assistant call:call_ask call:call_sub", "tool result:call_ask", "tool result:call_sub", "user input"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("messages %q\nwant     %q", got, want)
			}
		})

		t.Run(fmt.Sprintf("anthropic/stream=%v", stream), func(t *testing.T) {
			var bodies [][]byte
			client := &http.Client{Transport: anthropicHistoryTransport(func(r *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				bodies = append(bodies, body)
				payload := `{"id":"m1","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}`
				if strings.Contains(string(body), `"stream":true`) {
					payload = "event: message_start\ndata: {\"message\":{\"id\":\"m1\"}}\n\nevent: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"done\"}}\n\nevent: content_block_stop\ndata: {\"index\":0}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(payload)), Request: r}, nil
			})}
			cm, err := model.NewAnthropicChatModel(&model.AnthropicConfig{APIKey: "test", Model: "claude-sonnet-4-5", HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			run(t, cm, stream)
			if len(bodies) != 1 {
				t.Fatalf("requests = %d, want 1", len(bodies))
			}
			var req struct {
				Messages []struct {
					Role    string `json:"role"`
					Content []struct {
						Type      string          `json:"type"`
						ID        string          `json:"id"`
						ToolUseID string          `json:"tool_use_id"`
						Text      string          `json:"text"`
						Content   json.RawMessage `json:"content"`
					} `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(bodies[0], &req); err != nil {
				t.Fatal(err)
			}
			// Consecutive user messages form one turn; flatten each turn.
			var turns []string
			lastRole := ""
			for _, msg := range req.Messages {
				var parts []string
				for _, b := range msg.Content {
					switch b.Type {
					case "tool_use":
						parts = append(parts, "tool_use:"+b.ID)
					case "tool_result":
						parts = append(parts, "tool_result:"+b.ToolUseID)
						if !strings.Contains(string(b.Content), map[string]string{"call_ask": "did not run", "call_sub": "outcome is unknown"}[b.ToolUseID]) {
							t.Errorf("result for %s: %s", b.ToolUseID, b.Content)
						}
					case "text":
						if b.Text == interruptInput {
							parts = append(parts, "input")
						} else {
							parts = append(parts, "text")
						}
					}
				}
				if msg.Role == lastRole && len(turns) > 0 {
					turns[len(turns)-1] += " " + strings.Join(parts, " ")
				} else {
					turns = append(turns, msg.Role+": "+strings.Join(parts, " "))
				}
				lastRole = msg.Role
			}
			want := []string{"user: text", "assistant: tool_use:call_ask tool_use:call_sub", "user: tool_result:call_ask tool_result:call_sub input"}
			if !reflect.DeepEqual(turns, want) {
				t.Fatalf("turns %q\nwant  %q", turns, want)
			}
		})
	}
}

// A result in a message of its own still answers its call: hosts record
// results that way (openaiserver puts each in a separate message). A finished
// call without a result anywhere is left as it is.
func TestInterruptPairsResultsInOtherMessages(t *testing.T) {
	call := func(id string, state message.ToolCallState) message.ToolCallBlock {
		return message.ToolCallBlock{Type: "tool_call", ID: id, Name: "lookup", Input: `{}`, State: state}
	}
	result := message.ToolResultBlock{Type: "tool_result", ID: "h1", Name: "lookup", Output: "delivered", State: message.ToolResultSuccess}
	restored := []*message.Msg{
		message.UserMsg("user", "go"),
		message.AssistantMsg("bot", []message.ContentBlock{call("h1", message.ToolCallFinished), call("h2", message.ToolCallPending)}),
		message.AssistantMsg("tool", []message.ContentBlock{result}),
		message.NewMsg("tool", message.RoleUser, []message.ContentBlock{message.ToolResultBlock{
			Type: "tool_result", ID: "h2", Name: "lookup", Output: "ok", State: message.ToolResultSuccess}}),
		message.AssistantMsg("bot", []message.ContentBlock{call("h3", message.ToolCallFinished)}),
	}
	a := NewUnifiedAgent("bot", "helpful", &splitTailModel{},
		WithToolkit(tool.NewToolkit(tool.NewFunctionTool("lookup", "lookup", nil, func(context.Context, map[string]any) (any, error) {
			t.Error("a call ran")
			return "", nil
		}))),
		WithState(&AgentState{SchemaVersion: StateSchemaVersion, SessionID: "s", Context: restored}))
	for _, ev := range replyCollecting(t, a) {
		if ce, ok := ev.(event.CustomEvent); ok && ce.Name == ToolCallsInterruptedEvent {
			t.Fatalf("calls were closed: %v", ce.Value)
		}
	}
	got := renderSplitTail(a.state.Context)
	want := []string{`user: text("go")`, "assistant: call(h1) call(h2)", "assistant: result(h1)", "user: result(h2)", "assistant: call(h3)",
		fmt.Sprintf("user: text(%q)", interruptInput), `assistant: text("done")`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("context = %q\nwant      %q", got, want)
	}
}

// A canceled reply whose tool ignores cancellation keeps its core running
// after its stream ends. A reply started meanwhile leaves that core's call to
// it: the call gets only the result its tool returns.
func TestNewInputLeavesCallsOfARunningCore(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	slow := tool.NewFunctionTool("slow", "slow", nil, func(context.Context, map[string]any) (any, error) {
		close(started)
		<-release
		return "slow done", nil
	})
	m := &splitTailModel{script: []model.ChatResponse{{Content: []message.ContentBlock{
		message.ToolCallBlock{Type: "tool_call", ID: "tc_1", Name: "slow", Input: `{}`, State: message.ToolCallPending},
	}, IsLast: true}}}
	var mainCalls atomic.Int32
	var a *UnifiedAgent
	m.beforeMain = func() {
		if mainCalls.Add(1) != 2 {
			return
		}
		// The second reply's model call: its input is recorded. Let the
		// first reply's tool return, and wait for that core to end before
		// this reply goes on (overlapping cores share the turn statistics
		// without synchronization).
		close(release)
		deadline := time.Now().Add(5 * time.Second)
		for {
			a.mu.Lock()
			n := a.runningCores
			a.mu.Unlock()
			if n == 1 {
				return
			}
			if time.Now().After(deadline) {
				t.Error("the first reply's core did not return")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	a = NewUnifiedAgent("bot", "helpful", m, WithToolkit(tool.NewToolkit(slow)), WithReactConfig(ReactConfig{MaxIters: 3}))

	ctxA, cancelA := context.WithCancel(context.Background())
	chA, err := a.ReplyStream(ctxA, "first")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range chA {
		}
	}()
	<-started
	cancelA()
	for _, ev := range replyCollecting(t, a) {
		if ce, ok := ev.(event.CustomEvent); ok && ce.Name == ToolCallsInterruptedEvent {
			t.Errorf("the running core's call was closed: %v", ce.Value)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var results []string
	for _, msg := range a.state.Context {
		for _, b := range msg.Content {
			if r, ok := b.(message.ToolResultBlock); ok && r.ID == "tc_1" {
				results = append(results, r.GetOutputText())
			}
		}
	}
	if len(results) != 1 || results[0] != "slow done" {
		t.Fatalf("results for tc_1: %q, want only the tool's", results)
	}
}

// A reply started while an earlier reply still waits for an approval leaves
// that call to it: the new reply asks nothing, makes no model call and ends
// with the waiting message, and the earlier reply still takes the answer.
func TestNewInputLeavesApprovalOfARunningCore(t *testing.T) {
	var ran atomic.Int32
	lookup := tool.NewFunctionTool("lookup", "lookup", nil, func(context.Context, map[string]any) (any, error) {
		ran.Add(1)
		return "looked up", nil
	})
	m := &splitTailModel{script: []model.ChatResponse{{Content: []message.ContentBlock{
		message.ToolCallBlock{Type: "tool_call", ID: "tc_1", Name: "lookup", Input: `{}`, State: message.ToolCallPending},
	}, IsLast: true}}}
	a := NewUnifiedAgent("bot", "helpful", m, WithToolkit(tool.NewToolkit(lookup)),
		WithPermissionContext(permission.NewContext(permission.ModeDefault)), WithReactConfig(ReactConfig{MaxIters: 3}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chA, err := a.ReplyStream(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	var request event.RequireUserConfirmEvent
	for ev := range chA {
		if e, ok := ev.(event.RequireUserConfirmEvent); ok {
			request = e
			break
		}
	}
	if request.ReplyID == "" {
		t.Fatal("the first reply asked for no approval")
	}

	text := ""
	for _, ev := range replyCollecting(t, a) {
		switch e := ev.(type) {
		case event.ModelCallStartEvent:
			t.Error("the second reply called the model")
		case event.CustomEvent:
			if e.Name == ToolCallsInterruptedEvent {
				t.Errorf("the waiting call was closed: %v", e.Value)
			}
		case event.TextBlockDeltaEvent:
			text += e.Delta
		}
	}
	if text != "Waiting for user confirmation." {
		t.Errorf("the second reply said %q", text)
	}

	a.SubmitUserConfirm(&event.UserConfirmResultEvent{ReplyID: request.ReplyID,
		ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: request.ToolCalls[0]}}})
	for range chA {
	}
	if ctx.Err() != nil || ran.Load() != 1 {
		t.Fatalf("the approved call ran %d times (ctx %v), want once", ran.Load(), ctx.Err())
	}
}

// An answer the host queued for a call that a reply with new input closes is
// dropped, so a later call that reuses the ID (some providers number calls
// per response) is asked about itself and gets its own answer.
func TestInterruptDropsAnswersQueuedForClosedCalls(t *testing.T) {
	newCall := message.ToolCallBlock{Type: "tool_call", ID: "tc_1", Input: `{"q":"NEW"}`, State: message.ToolCallPending}
	restore := func(name string, state message.ToolCallState) AgentOption {
		return WithState(&AgentState{SchemaVersion: StateSchemaVersion, SessionID: "s", ReplyID: "old-reply", Context: []*message.Msg{
			message.UserMsg("user", "do the thing"),
			message.AssistantMsg("bot", []message.ContentBlock{
				message.ToolCallBlock{Type: "tool_call", ID: "tc_1", Name: name, Input: `{"q":"OLD"}`, State: state},
			}),
		}})
	}

	t.Run("confirmation", func(t *testing.T) {
		var ran []string
		lookup := tool.NewFunctionTool("lookup", "lookup", nil, func(_ context.Context, args map[string]any) (any, error) {
			ran = append(ran, fmt.Sprint(args["q"]))
			return "looked up", nil
		})
		next := newCall
		next.Name = "lookup"
		a := NewUnifiedAgent("bot", "helpful", &splitTailModel{script: []model.ChatResponse{{Content: []message.ContentBlock{next}, IsLast: true}}},
			WithToolkit(tool.NewToolkit(lookup)),
			WithPermissionContext(permission.NewContext(permission.ModeDefault)),
			WithReactConfig(ReactConfig{MaxIters: 3}),
			restore("lookup", message.ToolCallAsking))
		old := message.ToolCallBlock{Type: "tool_call", ID: "tc_1", Name: "lookup", Input: `{"q":"OLD"}`, State: message.ToolCallAsking}
		if err := a.SubmitUserConfirmContext(context.Background(), &event.UserConfirmResultEvent{
			ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: old}}}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ch, err := a.ReplyStream(ctx, interruptInput)
		if err != nil {
			t.Fatal(err)
		}
		asked := 0
		for ev := range ch {
			if e, ok := ev.(event.RequireUserConfirmEvent); ok {
				asked++
				go a.SubmitUserConfirm(&event.UserConfirmResultEvent{ReplyID: e.ReplyID,
					ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: e.ToolCalls[0]}}})
			}
		}
		if ctx.Err() != nil || asked != 1 || !reflect.DeepEqual(ran, []string{"NEW"}) {
			t.Fatalf("asked %d times, ran with %v (ctx %v); want one ask and a run with NEW", asked, ran, ctx.Err())
		}
	})

	t.Run("external result", func(t *testing.T) {
		next := newCall
		next.Name = "question"
		a := NewUnifiedAgent("bot", "helpful", &splitTailModel{script: []model.ChatResponse{{Content: []message.ContentBlock{next}, IsLast: true}}},
			WithToolkit(tool.NewToolkit(&validatedExternalTool{BaseTool: tool.BaseTool{ToolName: "question"}})),
			WithPermissionContext(permission.NewContext(permission.ModeDefault)),
			WithReactConfig(ReactConfig{MaxIters: 3}),
			restore("question", message.ToolCallSubmitted))
		if err := a.SubmitExternalResultContext(context.Background(), &event.ExternalExecutionResultEvent{
			ExecutionResults: []message.ToolResultBlock{{Type: "tool_result", ID: "tc_1", Name: "question", Output: "answer to OLD", State: message.ToolResultSuccess}}}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ch, err := a.ReplyStream(ctx, interruptInput)
		if err != nil {
			t.Fatal(err)
		}
		output := ""
		for ev := range ch {
			switch e := ev.(type) {
			case event.RequireExternalExecutionEvent:
				go a.SubmitExternalResult(&event.ExternalExecutionResultEvent{ReplyID: e.ReplyID,
					ExecutionResults: []message.ToolResultBlock{{Type: "tool_result", ID: "tc_1", Name: "question", Output: "answer to NEW", State: message.ToolResultSuccess}}})
			case event.ToolResultTextDeltaEvent:
				output += e.Delta
			}
		}
		if ctx.Err() != nil || output != "answer to NEW" {
			t.Fatalf("the new call got %q (ctx %v), want its own answer", output, ctx.Err())
		}
	})
}
