package tierrouter

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

func TestChatRouterConstructorValidation(t *testing.T) {
	validModel := func() model.ChatModel { return NewFakeChatModel(TierDevice) }
	valid := func() ChatRouterConfig {
		return ChatRouterConfig{
			Policy: StaticPolicy{Default: []Tier{TierDevice}}, Epoch: func() int64 { return 1 },
			Deployments: []ChatDeployment{{Tier: TierDevice, Model: validModel()}}, ContextFallback: 1024,
		}
	}
	var typedNil *FakeChatModel
	nested, err := NewChatRouter(valid())
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*ChatRouterConfig){
		"nil policy":      func(cfg *ChatRouterConfig) { cfg.Policy = nil },
		"nil epoch":       func(cfg *ChatRouterConfig) { cfg.Epoch = nil },
		"no deployments":  func(cfg *ChatRouterConfig) { cfg.Deployments = nil },
		"no fallback":     func(cfg *ChatRouterConfig) { cfg.ContextFallback = 0 },
		"nil model":       func(cfg *ChatRouterConfig) { cfg.Deployments[0].Model = nil },
		"typed nil model": func(cfg *ChatRouterConfig) { cfg.Deployments[0].Model = typedNil },
		"empty tier":      func(cfg *ChatRouterConfig) { cfg.Deployments[0].Tier = "" },
		"duplicate tier":  func(cfg *ChatRouterConfig) { cfg.Deployments = append(cfg.Deployments, cfg.Deployments[0]) },
		"nested router":   func(cfg *ChatRouterConfig) { cfg.Deployments[0].Model = nested },
		"fallback model": func(cfg *ChatRouterConfig) {
			cfg.Deployments[0].Model = model.NewFallbackChatModel(validModel(), validModel())
		},
		"connectivity model": func(cfg *ChatRouterConfig) {
			cfg.Deployments[0].Model = model.NewConnectivityAwareModel(validModel(), validModel())
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			mutate(&cfg)
			if _, err := NewChatRouter(cfg); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestChatRouterAdmissionRejections(t *testing.T) {
	tests := map[string]struct {
		op       *Operation
		policy   Policy
		epoch    int64
		cancel   bool
		want     error
		wantCode string
	}{
		"no route":       {op: &Operation{ID: "no-route", TaskEpoch: 1}, policy: StaticPolicy{}, epoch: 1, want: ErrNoRoute, wantCode: rejectNoRoute},
		"local no route": {op: &Operation{ID: "local-no-route", TaskEpoch: 1, LocalOnly: true}, policy: StaticPolicy{Default: []Tier{TierEdge}}, epoch: 1, want: ErrNoRoute, wantCode: rejectNoRoute},
		"stale":          {op: &Operation{ID: "stale", TaskEpoch: 1}, policy: StaticPolicy{Default: []Tier{TierDevice}}, epoch: 2, want: ErrStaleEpoch, wantCode: rejectStaleEpoch},
		"expired":        {op: &Operation{ID: "expired", TaskEpoch: 1, TTL: time.Second, SubmittedAt: time.Now().Add(-time.Minute)}, policy: StaticPolicy{Default: []Tier{TierDevice}}, epoch: 1, want: ErrExpired, wantCode: rejectExpired},
		"canceled":       {op: &Operation{ID: "canceled", TaskEpoch: 1}, policy: StaticPolicy{Default: []Tier{TierDevice}}, epoch: 1, cancel: true, want: context.Canceled, wantCode: stopCanceled},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			device := NewFakeChatModel(TierDevice)
			r := newChatHarness(t, tc.policy, func() int64 { return tc.epoch }, device)
			ctx := WithChatOperation(context.Background(), tc.op)
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := r.Chat(ctx, chatMessages("hello")); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			rec, _ := r.ChatSnapshot(tc.op.ID)
			if rec.State != StateRejected || rec.StopReason != tc.wantCode || len(device.Requests()) != 0 {
				t.Fatalf("record=%+v requests=%d", rec, len(device.Requests()))
			}
			if _, err := r.Chat(WithChatOperation(context.Background(), tc.op), chatMessages("hello")); !errors.Is(err, tc.want) {
				t.Fatalf("replay: %v", err)
			}
		})
	}
}

func TestReplayChatErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		rec  *ChatRecord
		want error
	}{
		{name: "pending", rec: &ChatRecord{ID: "x", State: StatePending}, want: ErrInFlight},
		{name: "no route", rec: &ChatRecord{ID: "x", State: StateRejected, StopReason: rejectNoRoute}, want: ErrNoRoute},
		{name: "rejected stale", rec: &ChatRecord{ID: "x", State: StateRejected, StopReason: rejectStaleEpoch}, want: ErrStaleEpoch},
		{name: "rejected expired", rec: &ChatRecord{ID: "x", State: StateRejected, StopReason: rejectExpired}, want: ErrExpired},
		{name: "rejected canceled", rec: &ChatRecord{ID: "x", State: StateRejected, StopReason: stopCanceled}, want: context.Canceled},
		{name: "exhausted", rec: &ChatRecord{ID: "x", State: StateExhausted}, want: ErrExhausted},
		{name: "superseded stale", rec: &ChatRecord{ID: "x", State: StateSuperseded, StopReason: rejectStaleEpoch}, want: ErrStaleEpoch},
		{name: "superseded expired", rec: &ChatRecord{ID: "x", State: StateSuperseded, StopReason: rejectExpired}, want: ErrExpired},
		{name: "superseded canceled", rec: &ChatRecord{ID: "x", State: StateSuperseded, StopReason: stopCanceled}, want: context.Canceled},
		{name: "uncertain", rec: &ChatRecord{ID: "x", State: StateUncertain}, want: ErrUncertain},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := replayChatError(tc.rec); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestChatUnsupportedRequests(t *testing.T) {
	device := NewFakeChatModel(TierDevice)
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierDevice}}, func() int64 { return 1 }, device)
	op := &Operation{ID: "unsupported-request", TaskEpoch: 1}
	ctx := WithChatOperation(context.Background(), op)
	optionTests := map[string][]model.CallOption{
		"nil option":          {nil},
		"tools":               {model.WithTools([]model.ToolSchema{{Type: "function"}})},
		"voice":               {model.WithVoice("alloy")},
		"retries":             {model.WithRetries(1, time.Second)},
		"tool choice":         {model.WithToolChoice(&model.ToolChoice{Mode: "required"})},
		"invalid option JSON": {model.WithTools([]model.ToolSchema{{Function: model.ToolFunction{Parameters: json.RawMessage("{")}}})},
	}
	for name, opts := range optionTests {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Chat(ctx, chatMessages("hello"), opts...); !errors.Is(err, ErrChatUnsupported) {
				t.Fatalf("want unsupported, got %v", err)
			}
		})
	}
	messageTests := map[string][]*message.Msg{
		"nil":        {nil},
		"tool block": {{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call"}}}},
		"metadata":   {{Metadata: types.JSONObject{"bad": func() {}}, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "x"}}}},
	}
	for name, msgs := range messageTests {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Chat(ctx, msgs); !errors.Is(err, ErrChatUnsupported) {
				t.Fatalf("want unsupported, got %v", err)
			}
		})
	}
	if len(device.Requests()) != 0 {
		t.Fatal("unsupported request dispatched")
	}
}

func TestCloneJSONValues(t *testing.T) {
	values := []any{nil, "text", true, json.Number("1.2"), float32(1), float64(2),
		int(-1), int8(1), int16(1), int32(1), int64(1), uint(1), uint8(1), uint16(1), uint32(1), uint64(1),
		[]any{map[string]any{"nested": "value"}}, types.JSONObject{"nested": []any{"value"}}}
	for _, value := range values {
		if _, err := cloneJSONValue(value); err != nil {
			t.Fatalf("clone %T: %v", value, err)
		}
	}
	for _, value := range []any{math.NaN(), float32(float64(math.Inf(1))), math.Inf(-1), json.Number("invalid"), func() {}} {
		if _, err := cloneJSONValue(value); err == nil {
			t.Fatalf("invalid value %T accepted", value)
		}
	}
	cloned, err := cloneJSONMap(nil)
	if err != nil || cloned != nil {
		t.Fatalf("nil map: %v %v", cloned, err)
	}
	var record *ChatRecord
	if record.clone() != nil {
		t.Fatal("nil record clone")
	}
	if _, ok := chatOperationFromContext(context.Background()); ok {
		t.Fatal("nil context has operation")
	}
}

func TestChatRouterMissingTierAndExhaustion(t *testing.T) {
	device := NewFakeChatModel(TierDevice)
	device.FailErr = errors.New("custom clean failure")
	device.FailNext = 1
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierDevice, TierEdge}}, func() int64 { return 1 }, device)
	op := &Operation{ID: "exhausted-chat", TaskEpoch: 1}
	if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); !errors.Is(err, ErrExhausted) {
		t.Fatalf("want exhausted, got %v", err)
	}
	rec, _ := r.ChatSnapshot(op.ID)
	if len(rec.Attempts) != 2 || rec.Attempts[0].Err != "custom clean failure" || rec.Attempts[1].Tier != TierEdge {
		t.Fatalf("attempts: %+v", rec.Attempts)
	}
	if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); !errors.Is(err, ErrExhausted) {
		t.Fatalf("exhausted replay: %v", err)
	}
}

func TestChatRouterUnknownContextAndReplayDamage(t *testing.T) {
	device := NewFakeChatModel(TierDevice)
	device.ContextWindow = 0
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierDevice}}, func() int64 { return 1 }, device)
	if got := r.ContextSize(); got != 1024 {
		t.Fatalf("unknown context fallback: %d", got)
	}
	if _, ok := r.ChatSnapshot("missing"); ok {
		t.Fatal("missing snapshot found")
	}
	op := &Operation{ID: "damaged-cache", TaskEpoch: 1}
	if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.operations[op.ID].response.Metadata["bad"] = func() {}
	r.mu.Unlock()
	if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); !errors.Is(err, ErrResponseUnavailable) {
		t.Fatalf("damaged cache: %v", err)
	}
}

func TestFakeChatModelCancellationAndRequestCopies(t *testing.T) {
	device := NewFakeChatModel(TierDevice)
	device.Delay = time.Second
	ctx, cancel := context.WithTimeout(WithChatOperation(context.Background(), &Operation{ID: "fake-delay", TaskEpoch: 1}), 20*time.Millisecond)
	defer cancel()
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierDevice}}, func() int64 { return 1 }, device)
	if _, err := r.Chat(ctx, chatMessages("hello")); !errors.Is(err, ErrUncertain) {
		t.Fatalf("delay cancellation: %v", err)
	}
	if _, err := device.ChatStream(context.Background(), nil); !errors.Is(err, model.ErrStreamNotSupported) {
		t.Fatalf("fake stream: %v", err)
	}
	requests := device.Requests()
	requests[0].Messages[0].Content[0] = message.TextBlock{Type: "text", Text: "mutated"}
	if got := device.Requests()[0].Messages[0].GetTextContent("\n"); got == nil || *got != "hello" {
		t.Fatalf("request copy mutated: %v", got)
	}
}
