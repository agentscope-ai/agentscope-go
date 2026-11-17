package retentioncopy

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

func copier(t *testing.T, limits Limits) *Copier {
	t.Helper()
	c, err := New(limits)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func copyOK(t *testing.T, c *Copier, v any) any {
	t.Helper()
	out, _, err := c.Copy(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, v) {
		t.Fatalf("type/value not preserved: %T -> %T", v, out)
	}
	return out
}

func TestSupportedValuesAndPointerForms(t *testing.T) {
	c := copier(t, testLimits())
	blocks := []message.ContentBlock{
		message.TextBlock{Type: "text", ID: "text", Text: "你好"},
		message.ThinkingBlock{Type: "thinking", Thinking: "thought", Extra: map[string]any{"signature": json.RawMessage(`{"keep":9007199254740993}`)}},
		message.RedactedThinkingBlock{Type: "redacted_thinking", Data: "opaque"},
		message.ToolCallBlock{Type: "tool_call", Name: "tool", Input: ` { "n":9007199254740993 } `, State: message.ToolCallAsking, SuggestedRules: []any{map[string]any{"mode": "ask"}}, Extra: map[string]any{"x": true}},
		message.ToolResultBlock{Type: "tool_result", State: message.ToolResultRunning, Output: []message.ContentBlock{message.TextBlock{Text: "output"}}, Metadata: map[string]any{"raw": []byte{0, 1}}},
		message.HintBlock{Type: "hint", Source: "host", Hint: []message.ContentBlock{message.TextBlock{Text: "hint"}}},
		message.DataBlock{Type: "data", Name: "image", Source: &message.URLSource{Type: "url", URL: "https://example.invalid/never-fetch", MediaType: "image/png"}},
	}
	values := []any{nil, true, false, "", int(-1), int8(-2), int16(-3), int32(-4), int64(9007199254740993), uint(1), uint8(2), uint16(3), uint32(4), uint64(math.MaxUint64), float32(1.25), float64(-0.25), math.Copysign(0, -1), json.Number("1e100000"), json.Number("-9007199254740993"), message.RoleUser, message.ToolCallFinished, message.ToolResultSuccess, message.ContentBlockText, json.RawMessage(" \xff\x00not-json "), []byte{0, 255}, []any{}, []string{"a", "b"}, map[string]string{"a": "b"}, types.JSONObject{"n": int64(9007199254740993)}, message.HintBlock{}, message.ToolResultBlock{}, message.Msg{}, message.Usage{InputTokens: 1, OutputTokens: 2, CacheCreationInputTokens: 3, CacheInputTokens: 4}, message.Base64Source{Type: "base64", Data: "not-decoded", MediaType: "image/png"}, message.URLSource{Type: "url", URL: "do-not-fetch"}}
	for _, v := range values {
		copyOK(t, c, v)
	}
	for _, b := range blocks {
		copyOK(t, c, b)
	}
	// Explicit pointer forms, including typed nils, must remain typed pointers.
	pointerValues := []any{&message.Msg{ID: "message", Role: message.RoleAssistant, Content: blocks, Metadata: types.JSONObject{"nested": []any{int64(7)}}, Timestamp: "time", FinishedAt: "end", Usage: &message.Usage{InputTokens: 1}}, &message.Usage{InputTokens: 3}, &message.TextBlock{Text: "text"}, &message.ThinkingBlock{Extra: map[string]any{"a": []any{"b"}}}, &message.RedactedThinkingBlock{Data: "redacted"}, &message.ToolCallBlock{SuggestedRules: []any{"rule"}}, &message.ToolResultBlock{Output: "result"}, &message.HintBlock{Hint: "hint"}, &message.DataBlock{Source: message.Base64Source{Data: "bytes"}}, &message.Base64Source{Data: "bytes"}, &message.URLSource{URL: "url"}, (*message.Msg)(nil), (*message.Usage)(nil), (*message.TextBlock)(nil), (*message.ThinkingBlock)(nil), (*message.RedactedThinkingBlock)(nil), (*message.ToolCallBlock)(nil), (*message.ToolResultBlock)(nil), (*message.HintBlock)(nil), (*message.DataBlock)(nil), (*message.Base64Source)(nil), (*message.URLSource)(nil)}
	for _, v := range pointerValues {
		out := copyOK(t, c, v)
		if !reflect.ValueOf(v).IsNil() && reflect.ValueOf(v).Pointer() == reflect.ValueOf(out).Pointer() {
			t.Fatalf("shared pointer %T", v)
		}
	}
	for _, v := range []any{([]any)(nil), ([]string)(nil), ([]byte)(nil), (json.RawMessage)(nil), ([]message.ContentBlock)(nil), ([]*message.Msg)(nil), (map[string]any)(nil), (map[string]string)(nil), (types.JSONObject)(nil), []byte{}, json.RawMessage{}, []message.ContentBlock{}, []*message.Msg{}, map[string]any{}, map[string]string{}, types.JSONObject{}, []*message.Msg{nil}} {
		copyOK(t, c, v)
	}
}

func TestNestedMutableFieldsAreDetached(t *testing.T) {
	c := copier(t, testLimits())
	source := &message.Msg{Content: []message.ContentBlock{
		&message.ToolCallBlock{SuggestedRules: []any{map[string]any{"rule": []string{"old"}}}, Extra: map[string]any{"bytes": json.RawMessage{1, 2}}},
		&message.ToolResultBlock{Output: []message.ContentBlock{&message.HintBlock{Hint: []message.ContentBlock{&message.TextBlock{Text: "old"}}}}, Metadata: map[string]any{"list": []any{true}}},
		&message.DataBlock{Source: &message.Base64Source{Data: "old"}},
	}}
	copied := copyOK(t, c, source).(*message.Msg)
	tc := copied.Content[0].(*message.ToolCallBlock)
	tc.SuggestedRules[0].(map[string]any)["rule"].([]string)[0] = "new"
	tc.Extra["bytes"].(json.RawMessage)[0] = 9
	originalCall := source.Content[0].(*message.ToolCallBlock)
	if originalCall.SuggestedRules[0].(map[string]any)["rule"].([]string)[0] != "old" || originalCall.Extra["bytes"].(json.RawMessage)[0] != 1 {
		t.Fatal("tool-call storage aliased")
	}
	result := source.Content[1].(*message.ToolResultBlock)
	result.Output.([]message.ContentBlock)[0].(*message.HintBlock).Hint.([]message.ContentBlock)[0].(*message.TextBlock).Text = "changed"
	result.Metadata["list"].([]any)[0] = false
	copiedResult := copied.Content[1].(*message.ToolResultBlock)
	if copiedResult.Output.([]message.ContentBlock)[0].(*message.HintBlock).Hint.([]message.ContentBlock)[0].(*message.TextBlock).Text != "old" || copiedResult.Metadata["list"].([]any)[0] != true {
		t.Fatal("tool-result/hint storage aliased")
	}
	copied.Content[2].(*message.DataBlock).Source.(*message.Base64Source).Data = "changed"
	if source.Content[2].(*message.DataBlock).Source.(*message.Base64Source).Data != "old" {
		t.Fatal("data source pointer shared")
	}
}

type trap struct{ called *bool }

func (v trap) MarshalJSON() ([]byte, error)      { *v.called = true; panic("must not marshal") }
func (v trap) String() string                    { *v.called = true; panic("must not stringify") }
func (v trap) GetType() message.ContentBlockType { *v.called = true; panic("must not call interface") }
func (v trap) GetID() string                     { *v.called = true; panic("must not call interface") }

type namedString string

func TestUnsupportedAndNumbers(t *testing.T) {
	c := copier(t, testLimits())
	called := false
	var nilTrap *trap
	for _, v := range []any{trap{&called}, &trap{&called}, nilTrap, map[string]any{"secret-key": trap{&called}}, []message.ContentBlock{trap{&called}}, namedString("secret"), complex(1, 2), uintptr(1), make(chan int), func() {}, map[int]string{1: "a"}, &map[string]any{}, &[]byte{}, &message.Msg{Metadata: types.JSONObject{"bad": trap{&called}}}} {
		out, _, err := c.Copy(context.Background(), v)
		if !errors.Is(err, ErrUnsupported) || out != nil {
			t.Fatalf("unsupported %T returned %T, %v", v, out, err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("error contains input")
		}
	}
	if called {
		t.Fatal("input callback called")
	}
	for _, v := range []any{math.NaN(), math.Inf(1), math.Inf(-1), float32(math.NaN()), float32(math.Inf(1)), json.Number(""), json.Number("NaN"), json.Number("null"), json.Number("1 "), json.Number(" 1"), json.Number("01"), json.Number("1e"), json.Number("1\n"), json.Number("[1]"), map[string]any{"n": math.NaN()}} {
		out, _, err := c.Copy(context.Background(), v)
		if out != nil || !errors.Is(err, ErrInvalidNumber) {
			t.Fatalf("invalid number %T accepted: %v", v, err)
		}
	}
}
func TestLimitsAndConfiguration(t *testing.T) {
	valid := testLimits()
	fields := []func(*Limits){func(l *Limits) { l.MaxMessages = 0 }, func(l *Limits) { l.MaxBlocks = -1 }, func(l *Limits) { l.MaxNodes = 0 }, func(l *Limits) { l.MaxPayloadBytes = 0 }, func(l *Limits) { l.MaxAllocBytes = -1 }, func(l *Limits) { l.MaxDepth = 0 }, func(l *Limits) { l.MaxDepth = 129 }}
	for _, change := range fields {
		l := valid
		change(&l)
		if c, err := New(l); c != nil || !errors.Is(err, ErrInvalidLimits) {
			t.Fatal("invalid limits accepted")
		}
	}
	var nilCopier *Copier
	for _, c := range []*Copier{nilCopier, {}} {
		if out, _, err := c.Copy(context.Background(), nil); out != nil || !errors.Is(err, ErrInvalidLimits) {
			t.Fatal("zero copier accepted")
		}
	}
	c := copier(t, valid)
	//nolint:staticcheck // Deliberately exercise the explicit nil-context rejection contract.
	if out, _, err := c.Copy(nil, nil); out != nil || !errors.Is(err, ErrInvalidContext) {
		t.Fatal("nil context accepted")
	}
	// The constructor stores a value, not a caller-owned configuration pointer.
	valid.MaxNodes = 1
	copyOK(t, c, []any{nil, nil})
}

func TestExactBudgetAndOneBelow(t *testing.T) {
	input := []*message.Msg{{Content: []message.ContentBlock{message.TextBlock{Text: "payload"}}, Metadata: types.JSONObject{"nested": []any{[]any{true}}}}}
	_, used, err := copier(t, testLimits()).Copy(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	exact := Limits{MaxMessages: used.Messages, MaxBlocks: used.Blocks, MaxNodes: used.Nodes, MaxPayloadBytes: used.PayloadBytes, MaxAllocBytes: used.AllocBytes, MaxDepth: used.Depth}
	copyOK(t, copier(t, exact), input)
	tests := []struct {
		resource string
		change   func(*Limits)
	}{
		{"messages", func(l *Limits) { l.MaxMessages = 1 }},
		{"blocks", func(l *Limits) { l.MaxBlocks = 1 }},
		{"nodes", func(l *Limits) { l.MaxNodes-- }},
		{"payload", func(l *Limits) { l.MaxPayloadBytes-- }},
		{"allocation", func(l *Limits) { l.MaxAllocBytes-- }},
		{"depth", func(l *Limits) { l.MaxDepth-- }},
	}
	for _, tt := range tests {
		t.Run(tt.resource, func(t *testing.T) {
			l := exact
			tt.change(&l)
			v := any(input)
			if tt.resource == "messages" {
				v = []*message.Msg{{}, {}}
			}
			if tt.resource == "blocks" {
				v = []message.ContentBlock{message.TextBlock{}, message.TextBlock{}}
			}
			if tt.resource == "messages" || tt.resource == "blocks" {
				l = testLimits()
				tt.change(&l)
			}
			out, stats, err := copier(t, l).Copy(context.Background(), v)
			var detail *Error
			if out != nil || !errors.Is(err, ErrLimit) || !errors.As(err, &detail) || detail.Resource != tt.resource {
				t.Fatalf("out=%T stats=%+v err=%v", out, stats, err)
			}
			if stats.Nodes > l.MaxNodes || stats.AllocBytes > l.MaxAllocBytes || stats.PayloadBytes > l.MaxPayloadBytes {
				t.Fatal("rejected reservation was charged")
			}
		})
	}
}

func TestWideContainersRejectedBeforeAllocation(t *testing.T) {
	for _, v := range []any{make([]any, 100000), map[string]any{"a": []any{}, "b": []any{}, "c": []any{}}} {
		l := testLimits()
		l.MaxNodes = 3
		out, stats, err := copier(t, l).Copy(context.Background(), v)
		if out != nil || !errors.Is(err, ErrLimit) || stats.AllocBytes != 16 || stats.Nodes != 1 {
			t.Fatalf("wide parent allocated before node admission: %+v %v", stats, err)
		}
	}
	l := testLimits()
	l.MaxAllocBytes = 64
	out, stats, err := copier(t, l).Copy(context.Background(), make([]any, 20))
	if out != nil || !errors.Is(err, ErrLimit) || stats.AllocBytes > 64 {
		t.Fatal("slice allocation not bounded")
	}
	// Empty containers still consume nodes and headers even without payload.
	v := []any{[]any{}, []any{}, []any{}, []any{}}
	l = testLimits()
	l.MaxAllocBytes = 128
	out, _, err = copier(t, l).Copy(context.Background(), v)
	if out != nil || !errors.Is(err, ErrLimit) {
		t.Fatal("empty containers not charged")
	}
}
func TestCyclesDepthAndRepeatedReferences(t *testing.T) {
	c := copier(t, testLimits())
	m := map[string]any{}
	m["cycle"] = m
	list := make([]any, 1)
	list[0] = list
	block := &message.HintBlock{}
	block.Hint = block
	for _, v := range []any{m, list, block} {
		out, _, err := c.Copy(context.Background(), v)
		if out != nil || !errors.Is(err, ErrLimit) {
			t.Fatal("cyclic input was not bounded")
		}
	}
	shared := map[string]any{"raw": []byte{1}}
	out, stats, err := c.Copy(context.Background(), []any{shared, shared})
	if err != nil {
		t.Fatal(err)
	}
	parts := out.([]any)
	parts[0].(map[string]any)["raw"].([]byte)[0] = 2
	if parts[1].(map[string]any)["raw"].([]byte)[0] != 1 || shared["raw"].([]byte)[0] != 1 {
		t.Fatal("graph identity retained")
	}
	_, one, err := c.Copy(context.Background(), []any{shared})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Nodes <= one.Nodes || stats.AllocBytes <= one.AllocBytes {
		t.Fatal("repeated copies not charged")
	}
	l := testLimits()
	l.MaxDepth = 1
	copyOK(t, copier(t, l), []any{nil})
	if out, _, err := copier(t, l).Copy(context.Background(), []any{[]any{nil}}); out != nil || !errors.Is(err, ErrLimit) {
		t.Fatal("depth boundary ignored")
	}
}

type cancelDuringCopy struct {
	context.Context
	checks int
}

func (c *cancelDuringCopy) Err() error {
	c.checks++
	if c.checks > 5 {
		return context.Canceled
	}
	return nil
}
func TestCancellationAndConcurrency(t *testing.T) {
	c := copier(t, testLimits())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, _, err := c.Copy(ctx, "input"); out != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
	during := &cancelDuringCopy{Context: context.Background()}
	if out, stats, err := c.Copy(during, []any{1, 2, 3, 4, 5, 6, 7}); out != nil || !errors.Is(err, context.Canceled) || stats.Nodes == 0 {
		t.Fatal("mid-copy cancellation returned a partial value")
	}
	input := map[string]any{"list": []any{json.Number("2")}}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, _, err := c.Copy(context.Background(), input)
			if err != nil {
				t.Error(err)
				return
			}
			out.(map[string]any)["list"].([]any)[0] = "private"
		}()
	}
	wg.Wait()
	if input["list"].([]any)[0] != json.Number("2") {
		t.Fatal("concurrent copy mutated input")
	}
}
func TestReservationOverflowAndLogicalAccounting(t *testing.T) {
	var used int64 = math.MaxInt64 - 1
	if err := reserve(&used, math.MaxInt64, 2, "nodes"); !errors.Is(err, ErrLimit) || used != math.MaxInt64-1 {
		t.Fatal("addition overflow")
	}
	b := &budget{limits: testLimits()}
	if err := b.slots(math.MaxInt, uintptr(math.MaxUint)); !errors.Is(err, ErrLimit) || b.stats.AllocBytes != 0 {
		t.Fatal("multiplication overflow")
	}
	c := copier(t, testLimits())
	_, s, err := c.Copy(context.Background(), []any{nil})
	if err != nil {
		t.Fatal(err)
	}
	// 2 visited values, slice header and one interface slot. No child double charge.
	if s.Nodes != 2 || s.AllocBytes != 32+int64(unsafe.Sizeof([]any{}))+int64(unsafe.Sizeof(any(nil))) {
		t.Fatalf("accounting=%+v", s)
	}
}

func TestFixedErrorAndDeadline(t *testing.T) {
	c := copier(t, testLimits())
	l := testLimits()
	l.MaxNodes = 1
	_, _, err := copier(t, l).Copy(context.Background(), []any{nil})
	if err == nil || err.Error() != "retention copy: resource limit exceeded (nodes)" {
		t.Fatalf("unexpected sanitized error: %v", err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Time{})
	defer cancel()
	if out, _, err := c.Copy(ctx, "data"); out != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("deadline cause lost")
	}
}
