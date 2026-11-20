package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestCanonicalJSON(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{`{"path": "a.go", "content": "x"}`, `{"content":"x","path":"a.go"}`, true},
		{`{"b":{"z":1,"a":[3,1]},"a":true}`, `{"a":true,"b":{"a":[3,1],"z":1}}`, true},
		{`{"n": 1.50, "big": 12345678901234567890}`, `{"big":12345678901234567890,"n":1.50}`, true},
		{`{"s": "<a href=\"x\">&</a>"}`, `{"s":"<a href=\"x\">&</a>"}`, true},
		{"{\"a\":1}\n  ", `{"a":1}`, true},
		{`[2, 1]`, `[2,1]`, true},
		{`{"a":1}}`, "", false},
		{`{} {}`, "", false},
		{`{"a":`, "", false},
		{``, "", false},
		{"{\"s\": \"\xff\"}", "", false},
		// Surrogate escapes, unpaired (decoding would give U+FFFD) or paired.
		{`{"s": "\ud800"}`, "", false},
		{`{"s": "x\uDFFF"}`, "", false},
		{`{"s": "\ud83d\ude00"}`, "", false},
		{`{"s": "\uDC00"}`, "", false},
		{`{"s": "\ud7ff"}`, "{\"s\":\"\ud7ff\"}", true},
		{`{"s": "\u00e9\ue000"}`, "{\"s\":\"é\ue000\"}", true},
		{`{"a": 1, "a": 2}`, `{"a":2}`, true},
		{`{"s": "\u00e9\/"}`, `{"s":"é/"}`, true},
	} {
		got, ok := canonicalJSON(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("canonicalJSON(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// localCall is a call in the local model's own form: path first, spaced,
// with provider metadata in Extra.
func localCall(id string) message.ToolCallBlock {
	return message.ToolCallBlock{Type: "tool_call", ID: id, Name: "write_file",
		Input: `{"path": "a.go", "content": "package a\n"}`,
		Extra: map[string]any{"call_id": id}}
}

// checkUnmarked fails if marking wrote into the provider's own Extra map.
func checkUnmarked(t *testing.T, call *message.ToolCallBlock) {
	t.Helper()
	if len(call.Extra) != 1 || call.Extra["call_id"] != call.ID {
		t.Fatalf("marking modified the model's own Extra: %v", call.Extra)
	}
}

const canonicalLocalInput = `{"content":"package a\n","path":"a.go"}`

// scriptedLocal answers every call with one tool call and records the
// histories it received.
type scriptedLocal struct {
	mu   sync.Mutex
	seen [][]*message.Msg
	call message.ToolCallBlock
}

func (s *scriptedLocal) record(msgs []*message.Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, msgs)
}

func (s *scriptedLocal) Chat(_ context.Context, msgs []*message.Msg, _ ...CallOption) (*ChatResponse, error) {
	s.record(msgs)
	return &ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "writing"}, s.call}, ModelName: "local"}, nil
}

func (s *scriptedLocal) ChatStream(ctx context.Context, msgs []*message.Msg, _ ...CallOption) (<-chan ChatResponse, error) {
	s.record(msgs)
	ch := make(chan ChatResponse, 2)
	ch <- ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "writing"}}}
	ch <- ChatResponse{IsLast: true, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "writing"}, s.call}}
	close(ch)
	return ch, nil
}

func (s *scriptedLocal) CountTokens([]*message.Msg, []ToolSchema) int { return 0 }

// cloudCapture is a Responses API server: it refuses the requests numbered
// in refuse (1-based) so the wrapper falls back to the local model, records
// the body of every other request and answers it with text, buffered or
// streamed as asked.
type cloudCapture struct {
	refuse map[int]bool

	mu     sync.Mutex
	n      int
	bodies []map[string]any
}

func (c *cloudCapture) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.n++
		refused := c.refuse[c.n]
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("cloud request is not JSON: %v", err)
		}
		if !refused {
			c.bodies = append(c.bodies, body)
		}
		c.mu.Unlock()
		if refused {
			http.Error(w, `{"error":{"message":"unavailable"}}`, http.StatusBadRequest)
			return
		}
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, sseDeltaLine("done"))
			fmt.Fprint(w, sseCompletedLine())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"r2","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
	}
}

// arguments returns the arguments of each function_call the request sent,
// by call id.
func arguments(t *testing.T, body map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	items, _ := body["input"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["type"] == "function_call" {
			id, _ := m["call_id"].(string)
			out[id], _ = m["arguments"].(string)
		}
	}
	return out
}

// checkPaired fails unless every function_call in a Responses request body
// has a function_call_output with the same call_id.
func checkPaired(t *testing.T, body map[string]any) {
	t.Helper()
	outputs := map[string]bool{}
	var calls []string
	items, _ := body["input"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		id, _ := m["call_id"].(string)
		switch m["type"] {
		case "function_call":
			calls = append(calls, id)
		case "function_call_output":
			outputs[id] = true
		}
	}
	if len(calls) == 0 {
		t.Fatal("the request carries no function_call")
	}
	for _, id := range calls {
		if !outputs[id] {
			t.Errorf("function_call %q has no function_call_output in the request", id)
		}
	}
}

// historyAfter is the conversation after the local model's reply, stored as
// UnifiedAgent stores one reply: a single assistant message holding a call
// the cloud wrote earlier (unmarked, in a non-canonical form) and its
// result, then the local model's reply and its result.
func historyAfter(local []message.ContentBlock) []*message.Msg {
	cloudOwn := message.ToolCallBlock{Type: "tool_call", ID: "call_cloud", Name: "write_file",
		Input: `{"path": "b.go", "content": "package b\n"}`}
	result := func(id string) message.ToolResultBlock {
		return message.ToolResultBlock{Type: "tool_result", ID: id, Name: "write_file",
			Output: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}}}
	}
	reply := []message.ContentBlock{cloudOwn, result("call_cloud")}
	reply = append(reply, local...)
	reply = append(reply, result("call_local"))
	return []*message.Msg{
		message.UserMsg("user", "write two files"),
		message.NewMsg("assistant", message.RoleAssistant, reply),
	}
}

func inputsOf(msgs []*message.Msg) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Content {
			if c, ok := b.(message.ToolCallBlock); ok {
				out = append(out, c.Input)
			}
		}
	}
	return out
}

// TestCanonicalReplayWire drives a ConnectivityAwareModel and a
// FallbackChatModel with a real Responses adapter as the cloud (the primary):
// the local model's call reaches the cloud's wire with sorted keys, the
// cloud's own call as written, and the caller's history is not modified.
// When the cloud fails again, the local model receives both calls as
// written. Without the option the local model's form goes out unchanged.
func TestCanonicalReplayWire(t *testing.T) {
	for _, wrapper := range []string{"connectivity", "fallback"} {
		for _, tc := range []struct {
			name      string
			canonical bool
			stream    bool
		}{
			{"chat", true, false},
			{"stream", true, true},
			{"off", false, false},
			{"off stream", false, true},
		} {
			t.Run(wrapper+" "+tc.name, func(t *testing.T) {
				testCanonicalReplayWire(t, wrapper, tc.canonical, tc.stream)
			})
		}
	}
}

func testCanonicalReplayWire(t *testing.T, wrapper string, canonical, stream bool) {
	// Requests 1 and 3 fail, so the first and the third call go to the
	// local model and the second to the cloud.
	cc := &cloudCapture{refuse: map[int]bool{1: true, 3: true}}
	cloud := newResponsesStreamModel(t, cc.handler(t))
	local := &scriptedLocal{call: localCall("call_local")}
	var m ChatModel
	var cam *ConnectivityAwareModel
	if wrapper == "connectivity" {
		opts := []ConnectivityOption{WithFailureThreshold(1), WithRecoveryTimeout(time.Nanosecond)}
		if canonical {
			opts = append(opts, WithCanonicalReplay())
		}
		cam = NewConnectivityAwareModel(local, cloud, opts...)
		m = cam
	} else {
		f := NewFallbackChatModel(cloud, local)
		f.CanonicalReplay = canonical
		m = f
	}
	ctx := context.Background()
	first := []*message.Msg{message.UserMsg("user", "write two files")}

	call := func(msgs []*message.Msg) *ChatResponse {
		t.Helper()
		if !stream {
			resp, err := m.Chat(ctx, msgs)
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			return resp
		}
		ch, err := m.ChatStream(ctx, msgs)
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		var last *ChatResponse
		for r := range ch {
			if r.Error != nil {
				t.Fatalf("stream error: %v", r.Error)
			}
			if r.IsLast {
				r := r
				last = &r
			}
		}
		if last == nil {
			t.Fatal("stream ended without a final response")
		}
		return last
	}

	resp := call(first)
	var got message.ToolCallBlock
	for _, b := range resp.Content {
		if c, ok := b.(message.ToolCallBlock); ok {
			got = c
		}
	}
	rank, marked := writerRank(got.Extra)
	if canonical != marked || (marked && rank != 1) {
		t.Fatalf("local call mark = %v, %v; want marked %v with rank 1", rank, marked, canonical)
	}
	if got.Input != localCall("").Input {
		t.Fatalf("the local call returned to the caller changed: %q", got.Input)
	}
	if got.Extra["call_id"] != "call_local" {
		t.Fatalf("marking dropped the provider's metadata: %v", got.Extra)
	}
	checkUnmarked(t, &local.call)

	hist := historyAfter(resp.Content)
	before := inputsOf(hist)
	if cam != nil {
		// The recovery timeout is a nanosecond, but a coarse clock can
		// report no time passed: wait until the probe is due.
		for deadline := time.Now().Add(5 * time.Second); cam.ActiveModel() != "cloud"; {
			if time.Now().After(deadline) {
				t.Fatal("the circuit never became half-open")
			}
			time.Sleep(time.Millisecond)
		}
	}
	call(hist)

	cc.mu.Lock()
	bodies := cc.bodies
	cc.mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("cloud saw %d requests after the refusal, want 1", len(bodies))
	}
	checkPaired(t, bodies[0])
	args := arguments(t, bodies[0])
	wantLocal := localCall("").Input
	if canonical {
		wantLocal = canonicalLocalInput
	}
	if args["call_local"] != wantLocal {
		t.Errorf("local call reached the cloud as %q, want %q", args["call_local"], wantLocal)
	}
	if args["call_cloud"] != `{"path": "b.go", "content": "package b\n"}` {
		t.Errorf("the cloud's own call changed on the wire: %q", args["call_cloud"])
	}
	if after := inputsOf(hist); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("the caller's history changed: %q -> %q", before, after)
	}

	// The cloud fails again: the local model receives the history holding
	// its own marked call, and the cloud's, as written.
	call(append(append([]*message.Msg(nil), hist...), message.UserMsg("user", "and a third")))
	cc.mu.Lock()
	cloudCalls := len(cc.bodies)
	cc.mu.Unlock()
	if cloudCalls != 1 {
		t.Fatalf("cloud answered %d requests, want 1", cloudCalls)
	}
	local.mu.Lock()
	defer local.mu.Unlock()
	if len(local.seen) != 2 || inputsOf(local.seen[0]) != nil {
		t.Fatalf("local model calls = %d", len(local.seen))
	}
	if got := inputsOf(local.seen[1]); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Errorf("the local model received %q, want its history as written %q", got, before)
	}
	checkUnmarked(t, &local.call)
}

// TestCanonicalForSurvivesCheckpoint checks that the mark still works after
// the messages went through JSON, where the rank becomes a float64.
func TestCanonicalForSurvivesCheckpoint(t *testing.T) {
	resp := markWriter(&ChatResponse{Content: []message.ContentBlock{localCall("c1")}}, 1)
	msg := message.NewMsg("assistant", message.RoleAssistant, resp.Content)
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var back message.Msg
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	out := canonicalFor([]*message.Msg{&back}, 0)
	if got := inputsOf(out); len(got) != 1 || got[0] != canonicalLocalInput {
		t.Fatalf("after a round trip the call is sent as %q", got)
	}
	if out[0] == &back {
		t.Fatal("canonicalFor must copy a message it rewrites")
	}
	if got := inputsOf([]*message.Msg{&back}); got[0] != localCall("").Input {
		t.Fatalf("the restored message changed: %q", got[0])
	}
}

func TestCanonicalForLeavesOtherCallsAlone(t *testing.T) {
	broken := message.ToolCallBlock{Type: "tool_call", ID: "b", Name: "write_file", Input: `{"path": "a.go", "content": "unterminated`,
		Extra: map[string]any{writerRankKey: 2}}
	canonicalAlready := message.ToolCallBlock{Type: "tool_call", ID: "c", Name: "read_file", Input: `{"path":"a.go"}`,
		Extra: map[string]any{writerRankKey: 2}}
	own := message.ToolCallBlock{Type: "tool_call", ID: "o", Name: "write_file", Input: `{"path": "x", "content": "y"}`,
		Extra: map[string]any{writerRankKey: 1}}
	msgs := []*message.Msg{
		nil,
		message.NewMsg("assistant", message.RoleAssistant, []message.ContentBlock{broken, canonicalAlready, own}),
	}
	out := canonicalFor(msgs, 1)
	if &out[0] != &msgs[0] {
		t.Fatal("a history with nothing to rewrite must be returned as is")
	}
	if got := canonicalFor(msgs, 0); inputsOf(got[1:])[2] != `{"content":"y","path":"x"}` {
		t.Fatalf("a rank-1 call sent to rank 0 = %q", inputsOf(got[1:]))
	}
}

func TestFallbackCanonicalReplay(t *testing.T) {
	var mu sync.Mutex
	received := map[string][][]string{}
	member := func(name string, fail *int, call *message.ToolCallBlock) ChatModel {
		return &mockChatModel{chatFn: func(_ context.Context, msgs []*message.Msg, _ ...CallOption) (*ChatResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			received[name] = append(received[name], inputsOf(msgs))
			if fail != nil && *fail > 0 {
				*fail--
				return nil, fmt.Errorf("%s down", name)
			}
			resp := &ChatResponse{ModelName: name}
			if call != nil {
				resp.Content = []message.ContentBlock{*call}
			}
			return resp, nil
		}}
	}
	failPrimary, failSecond := 1, 1
	second := message.ToolCallBlock{Type: "tool_call", ID: "s", Name: "write_file", Input: `{"path": "s", "content": "1"}`}
	third := message.ToolCallBlock{Type: "tool_call", ID: "t", Name: "write_file", Input: `{"path": "t", "content": "2"}`}
	f := NewFallbackChain(member("primary", &failPrimary, nil), member("second", &failSecond, &second), member("third", nil, &third))
	f.CanonicalReplay = true
	ctx := context.Background()

	// The primary and the second member fail; the third writes a call.
	r1, err := f.Chat(ctx, []*message.Msg{message.UserMsg("u", "go")})
	if err != nil {
		t.Fatal(err)
	}
	if rank, _ := writerRank(r1.Content[0].(message.ToolCallBlock).Extra); rank != 2 {
		t.Fatalf("third member's call rank = %d, want 2", rank)
	}
	// Now the primary is up again, but the next call goes to the second
	// member only when the primary fails: make it fail once more.
	failPrimary = 1
	hist := []*message.Msg{message.NewMsg("assistant", message.RoleAssistant, r1.Content)}
	r2, err := f.Chat(ctx, hist)
	if err != nil {
		t.Fatal(err)
	}
	if rank, _ := writerRank(r2.Content[0].(message.ToolCallBlock).Extra); rank != 1 {
		t.Fatalf("second member's call rank = %d, want 1", rank)
	}
	hist = append(hist, message.NewMsg("assistant", message.RoleAssistant, r2.Content))
	if _, err := f.Chat(ctx, hist); err != nil {
		t.Fatal(err)
	}

	want := map[string][][]string{
		// Call 2 reaches the primary with the third member's call rewritten;
		// call 3 with both fallback members' calls rewritten.
		"primary": {nil, {`{"content":"2","path":"t"}`}, {`{"content":"2","path":"t"}`, `{"content":"1","path":"s"}`}},
		// The second member sees the third member's call rewritten.
		"second": {nil, {`{"content":"2","path":"t"}`}},
		"third":  {nil},
	}
	if fmt.Sprint(received) != fmt.Sprint(want) {
		t.Fatalf("histories received:\n got %v\nwant %v", received, want)
	}
}

func TestFallbackCanonicalReplayOffSendsAsWritten(t *testing.T) {
	var got []string
	primary := &mockChatModel{chatFn: func(_ context.Context, msgs []*message.Msg, _ ...CallOption) (*ChatResponse, error) {
		got = inputsOf(msgs)
		return &ChatResponse{}, nil
	}}
	marked := localCall("c")
	marked.Extra = map[string]any{writerRankKey: 1}
	f := NewFallbackChatModel(primary, primary)
	if _, err := f.Chat(context.Background(), []*message.Msg{message.NewMsg("assistant", message.RoleAssistant, []message.ContentBlock{marked})}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != localCall("").Input {
		t.Fatalf("with the option off the call went out as %q", got)
	}
}

// TestMarkWriterStreamStopsOnCancel checks that the forwarder closes its
// channel when the caller's context ends, without waiting for the source.
func TestMarkWriterStreamStopsOnCancel(t *testing.T) {
	in := make(chan ChatResponse)
	ctx, cancel := context.WithCancel(context.Background())
	out := markWriterStream(ctx, in, 1)
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("no response was sent, yet one arrived")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the forwarder did not stop on cancellation")
	}
}

// streamMember streams one final response with the given call, or fails at
// setup.
type streamMember struct {
	fail bool
	call *message.ToolCallBlock
	got  []string
}

func (s *streamMember) Chat(context.Context, []*message.Msg, ...CallOption) (*ChatResponse, error) {
	return nil, fmt.Errorf("buffered calls are not used")
}

func (s *streamMember) ChatStream(_ context.Context, msgs []*message.Msg, _ ...CallOption) (<-chan ChatResponse, error) {
	s.got = inputsOf(msgs)
	if s.fail {
		return nil, fmt.Errorf("setup failed")
	}
	ch := make(chan ChatResponse, 1)
	r := ChatResponse{IsLast: true}
	if s.call != nil {
		r.Content = []message.ContentBlock{*s.call}
	}
	ch <- r
	close(ch)
	return ch, nil
}

func (s *streamMember) CountTokens([]*message.Msg, []ToolSchema) int { return 0 }

func TestFallbackCanonicalReplayStream(t *testing.T) {
	call := localCall("l")
	primary := &streamMember{fail: true}
	fallback := &streamMember{call: &call}
	f := NewFallbackChatModel(primary, fallback)
	f.CanonicalReplay = true
	marked := localCall("old")
	marked.Extra = map[string]any{writerRankKey: 1}
	hist := []*message.Msg{message.NewMsg("assistant", message.RoleAssistant, []message.ContentBlock{marked})}
	ch, err := f.ChatStream(context.Background(), hist)
	if err != nil {
		t.Fatal(err)
	}
	var last ChatResponse
	for r := range ch {
		last = r
	}
	if rank, _ := writerRank(last.Content[0].(message.ToolCallBlock).Extra); rank != 1 {
		t.Fatalf("the fallback's streamed call rank = %d, want 1", rank)
	}
	if len(primary.got) != 1 || primary.got[0] != canonicalLocalInput {
		t.Fatalf("the primary's stream request carried %q", primary.got)
	}
	if len(fallback.got) != 1 || fallback.got[0] != localCall("").Input {
		t.Fatalf("the fallback's own call must reach it as written: %q", fallback.got)
	}
	checkUnmarked(t, &call)
}

func TestWriterRankForms(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want int
		ok   bool
	}{
		{1, 1, true}, {float64(2), 2, true}, {json.Number("3"), 3, true},
		{json.Number("x"), 0, false}, {"1", 0, false}, {nil, 0, false},
	} {
		got, ok := writerRank(map[string]any{writerRankKey: tc.v})
		if got != tc.want || ok != tc.ok {
			t.Errorf("writerRank(%#v) = %d, %v; want %d, %v", tc.v, got, ok, tc.want, tc.ok)
		}
	}
	if markWriter(nil, 1) != nil {
		t.Error("markWriter(nil) must stay nil")
	}
	plain := &ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "x"}}}
	if markWriter(plain, 1) != plain || markWriter(plain, 0) != plain {
		t.Error("a response without calls, or rank 0, is returned as is")
	}
	in := make(chan ChatResponse)
	if markWriterStream(context.Background(), in, 0) != (<-chan ChatResponse)(in) {
		t.Error("rank 0 streams are returned as is")
	}
}

// TestMarkWriterStreamStopsWhileSending covers a consumer that stops reading
// after the source produced a response.
func TestMarkWriterStreamStopsWhileSending(t *testing.T) {
	in := make(chan ChatResponse, 1)
	in <- ChatResponse{IsLast: true}
	ctx, cancel := context.WithCancel(context.Background())
	out := markWriterStream(ctx, in, 1)
	time.Sleep(10 * time.Millisecond) // let the forwarder block on the send
	cancel()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-out:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the forwarder did not close after cancellation")
		}
	}
}

// TestMarkWriterStreamDrainsAfterCancel covers a producer that ignores
// cancellation: once the caller's context ends and nobody reads the
// returned channel, the forwarder still drains the source so the producer
// can finish.
func TestMarkWriterStreamDrainsAfterCancel(t *testing.T) {
	in := make(chan ChatResponse)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 3 {
			in <- ChatResponse{}
		}
		in <- ChatResponse{IsLast: true}
		close(in)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	out := markWriterStream(ctx, in, 1)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the producer stayed blocked after cancellation")
	}
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("a response arrived after cancellation, with nobody reading")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the forwarder did not close after cancellation")
	}
}
