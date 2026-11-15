package tierrouter

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

func TestChatRejectsInvalidUTF8WithoutReplay(t *testing.T) {
	device := NewFakeChatModel(TierDevice)
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierDevice}}, func() int64 { return 1 }, device)
	op := &Operation{ID: "utf8-operation", TaskEpoch: 1, Kind: "chat", Content: "valid"}
	if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"ff": "\xff", "fe": "\xfe"} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages(text)); !errors.Is(err, ErrChatUnsupported) {
				t.Fatalf("invalid UTF-8: %v", err)
			}
		})
	}
	for name, op := range map[string]*Operation{
		"id":      {ID: "bad-\xff", TaskEpoch: 1},
		"kind":    {ID: "bad-kind", TaskEpoch: 1, Kind: "\xfe"},
		"content": {ID: "bad-content", TaskEpoch: 1, Content: "\xff"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); !errors.Is(err, ErrChatUnsupported) {
				t.Fatalf("invalid operation: %v", err)
			}
		})
	}
	if len(device.Requests()) != 1 {
		t.Fatalf("invalid request replayed or dispatched: %d", len(device.Requests()))
	}
}

func TestChatRejectsCyclicMetadata(t *testing.T) {
	cyclicMap := map[string]any{}
	cyclicMap["self"] = cyclicMap
	cyclicSlice := make([]any, 1)
	cyclicSlice[0] = cyclicSlice
	for name, value := range map[string]any{"map": cyclicMap, "slice": cyclicSlice} {
		t.Run("request "+name, func(t *testing.T) {
			device := NewFakeChatModel(TierDevice)
			r := newChatHarness(t, StaticPolicy{Default: []Tier{TierDevice}}, func() int64 { return 1 }, device)
			msgs := chatMessages("hello")
			msgs[0].Metadata = types.JSONObject{"cycle": value}
			op := &Operation{ID: "request-cycle-" + name, TaskEpoch: 1}
			if _, err := r.Chat(WithChatOperation(context.Background(), op), msgs); !errors.Is(err, ErrChatUnsupported) {
				t.Fatalf("cyclic request: %v", err)
			}
			if len(device.Requests()) != 0 {
				t.Fatal("cyclic request dispatched")
			}
		})
		t.Run("response "+name, func(t *testing.T) {
			cloud := NewFakeChatModel(TierCloud)
			cloud.ReturnResponse = true
			cloud.Response = &model.ChatResponse{
				Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "answer"}}, IsLast: true,
				Metadata: map[string]any{"cycle": value},
			}
			edge := NewFakeChatModel(TierEdge)
			r := newChatHarness(t, StaticPolicy{Default: []Tier{TierCloud, TierEdge}}, func() int64 { return 1 }, cloud, edge)
			op := &Operation{ID: "response-cycle-" + name, TaskEpoch: 1}
			if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); !errors.Is(err, ErrChatInvalidResponse) {
				t.Fatalf("cyclic response: %v", err)
			}
			if len(cloud.Requests()) != 1 || len(edge.Requests()) != 0 {
				t.Fatalf("cloud=%d edge=%d", len(cloud.Requests()), len(edge.Requests()))
			}
		})
	}
}

func TestChatPreservesTypedNilMetadata(t *testing.T) {
	metadata := map[string]any{"slice": []any(nil), "object": types.JSONObject(nil)}
	cloud := NewFakeChatModel(TierCloud)
	cloud.ReturnResponse = true
	cloud.Response = &model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "answer"}}, IsLast: true,
		Metadata: metadata,
	}
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierCloud}}, func() int64 { return 1 }, cloud)
	msgs := chatMessages("hello")
	msgs[0].Metadata = types.JSONObject{"slice": []any(nil), "object": types.JSONObject(nil)}
	op := &Operation{ID: "typed-nil", TaskEpoch: 1}
	resp, err := r.Chat(WithChatOperation(context.Background(), op), msgs)
	if err != nil {
		t.Fatal(err)
	}
	assertTypedNil := func(t *testing.T, values map[string]any) {
		t.Helper()
		if values["slice"] == nil || !reflect.ValueOf(values["slice"]).IsNil() {
			t.Fatalf("nil slice changed: %#v", values["slice"])
		}
		if values["object"] == nil || !reflect.ValueOf(values["object"]).IsNil() {
			t.Fatalf("nil object changed: %#v", values["object"])
		}
	}
	assertTypedNil(t, resp.Metadata)
	requestMetadata := cloud.Requests()[0].Messages[0].Metadata
	assertTypedNil(t, map[string]any{"slice": requestMetadata["slice"], "object": requestMetadata["object"]})
	op = &Operation{ID: "top-level-nil", TaskEpoch: 1}
	if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); err != nil {
		t.Fatal(err)
	}
	if got := cloud.Requests()[1].Messages[0].Metadata; got != nil {
		t.Fatalf("top-level nil metadata changed: %#v", got)
	}
}

func TestChatRejectsInvalidMetadataUTF8(t *testing.T) {
	device := NewFakeChatModel(TierDevice)
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierDevice}}, func() int64 { return 1 }, device)
	for name, metadata := range map[string]types.JSONObject{
		"key":   {"bad-\xff": "value"},
		"value": {"key": "bad-\xfe"},
	} {
		t.Run(name, func(t *testing.T) {
			msgs := chatMessages("hello")
			msgs[0].Metadata = metadata
			op := &Operation{ID: "metadata-utf8-" + name, TaskEpoch: 1}
			if _, err := r.Chat(WithChatOperation(context.Background(), op), msgs); !errors.Is(err, ErrChatUnsupported) {
				t.Fatalf("metadata UTF-8: %v", err)
			}
		})
	}
	if len(device.Requests()) != 0 {
		t.Fatal("invalid metadata dispatched")
	}
}

func TestChatRejectsInvalidOptionUTF8(t *testing.T) {
	device := NewFakeChatModel(TierDevice)
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierDevice}}, func() int64 { return 1 }, device)
	tests := map[string][]model.CallOption{
		"reasoning":         {model.WithReasoningEffort("\xff")},
		"format type":       {model.WithResponseFormat(&model.ResponseFormat{Type: "\xfe"})},
		"format name":       {model.WithResponseFormat(&model.ResponseFormat{Name: "\xff"})},
		"tool choice mode":  {model.WithToolChoice(&model.ToolChoice{Mode: "\xfe"})},
		"tool choice tools": {model.WithToolChoice(&model.ToolChoice{Mode: "none", Tools: []string{"\xff"}})},
	}
	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			op := &Operation{ID: "option-utf8-" + name, TaskEpoch: 1}
			if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello"), opts...); !errors.Is(err, ErrChatUnsupported) {
				t.Fatalf("invalid option: %v", err)
			}
		})
	}
	if len(device.Requests()) != 0 {
		t.Fatal("invalid options dispatched")
	}
}

func TestCloneJSONAllowsAcyclicSliceViewsAndSharedBranches(t *testing.T) {
	nestedEmpty := make([]any, 1)
	nestedEmpty[0] = nestedEmpty[:0]
	if _, err := cloneJSONValue(nestedEmpty); err != nil {
		t.Fatalf("nested empty slice rejected: %v", err)
	}
	prefix := make([]any, 2)
	prefix[0] = "value"
	prefix[1] = prefix[:1]
	if _, err := cloneJSONValue(prefix); err != nil {
		t.Fatalf("overlapping prefix rejected: %v", err)
	}
	shared := map[string]any{"value": "shared"}
	if _, err := cloneJSONValue([]any{shared, shared}); err != nil {
		t.Fatalf("shared sibling rejected: %v", err)
	}
}
