package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// This is the public-Reply reproduction from #11. Its only real adapter
// behavior is CountTokens: no provider, quality, or savings claim is implied.
type blockCompactionFixture struct {
	counter    model.ChatModel
	rounds     int
	summaries  int
	inputs     [][]string
	compressed [][]string
}

func (f *blockCompactionFixture) CountTokens(msgs []*message.Msg, tools []model.ToolSchema) int {
	return f.counter.CountTokens(msgs, tools)
}

func (f *blockCompactionFixture) Chat(_ context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	var cfg model.CallOptions
	for _, opt := range opts {
		opt(&cfg)
	}
	if len(cfg.Tools) == 1 && cfg.Tools[0].Function.Name == "generate_structured_output" {
		f.summaries++
		f.compressed = append(f.compressed, blockResultIDs(msgs))
		return summaryToolCallResponse(&model.ChatUsage{InputTokens: 11, OutputTokens: 7}), nil
	}
	f.inputs = append(f.inputs, blockResultIDs(msgs))
	f.rounds++
	if f.rounds <= 4 {
		return &model.ChatResponse{IsLast: true, Usage: &model.ChatUsage{InputTokens: 2, OutputTokens: 3}, Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: fmt.Sprintf("read-%d", f.rounds), Name: "read_log", Input: `{}`, State: message.ToolCallPending},
		}}, nil
	}
	return &model.ChatResponse{IsLast: true, Usage: &model.ChatUsage{InputTokens: 2, OutputTokens: 3}, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}}, nil
}

func (*blockCompactionFixture) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func blockResultIDs(msgs []*message.Msg) []string {
	var ids []string
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		for _, block := range msg.Content {
			if result, ok := block.(message.ToolResultBlock); ok {
				ids = append(ids, result.ID)
			}
		}
	}
	return ids
}

func TestContentBlockCompactionPublicReply(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/opt-in=%t", stream, enabled), func(t *testing.T) {
				counter, err := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "offline-fixture", Model: "fixture"})
				if err != nil {
					t.Fatal(err)
				}
				f := &blockCompactionFixture{counter: counter}
				executions := 0
				read := tool.NewFunctionTool("read_log", "Read one diagnostic log", json.RawMessage(`{"type":"object","properties":{}}`),
					func(context.Context, map[string]any) (any, error) {
						executions++
						return strings.Repeat("INFO request processed latency_ms=23 status=200\n", 90), nil
					})
				state := &AgentState{}
				opts := []AgentOption{WithState(state), WithToolkit(tool.NewToolkit(read)), WithModelStreaming(false),
					WithReactConfig(ReactConfig{MaxIters: 12}), WithModelConfig(ModelConfig{MaxRetries: 1}),
					WithContextConfig(&ContextConfig{ContextSize: 4096, TriggerRatio: .8, ReserveRatio: .3, ToolResultLimit: 1200})}
				if enabled {
					opts = append(opts, WithContentBlockCompaction())
				}
				a := NewUnifiedAgent("analyst", "Analyze diagnostic logs.", f, opts...)
				var reply *message.Msg
				if stream {
					ch, streamErr := a.ReplyStream(context.Background(), "Inspect four logs, then report findings.")
					if streamErr != nil {
						t.Fatal(streamErr)
					}
					for ev := range ch {
						if end, ok := ev.(event.ReplyEndEvent); ok && end.Error != nil {
							t.Fatal(end.Error)
						}
					}
				} else {
					reply, err = a.Reply(context.Background(), "Inspect four logs, then report findings.")
					if err != nil {
						t.Fatal(err)
					}
					if reply == nil || reply.ID != state.ReplyID {
						t.Fatalf("reply identity: %#v", reply)
					}
				}
				if executions != 4 || f.rounds != 5 || f.summaries == 0 {
					t.Fatalf("tools=%d rounds=%d summaries=%d", executions, f.rounds, f.summaries)
				}
				var want []string
				if enabled {
					// The third result triggers compaction. The fourth fits below
					// the next trigger, so both observations remain verbatim.
					want = []string{"read-3", "read-4"}
				}
				if got := blockResultIDs(state.Context); !reflect.DeepEqual(got, want) {
					t.Fatalf("retained results = %v, want %v", got, want)
				}
				if got := f.inputs[len(f.inputs)-1]; !reflect.DeepEqual(got, want) {
					t.Fatalf("last reasoning input = %v, want %v", got, want)
				}
				if enabled {
					foundPrefix := false
					for _, ids := range f.compressed {
						if len(ids) > 0 {
							foundPrefix = true
						}
					}
					if !foundPrefix {
						t.Fatal("no completed tool prefix was summarized")
					}
					if reply != nil && reply.Usage == nil {
						t.Fatal("reply usage was lost")
					}
				}
			})
		}
	}
}

// An independent block-weight estimate makes the direct boundary fixtures
// readable: each content block costs one, as does each supplied tool schema.
type blockCountModel struct{ compressionMockModel }

func (*blockCountModel) CountTokens(msgs []*message.Msg, tools []model.ToolSchema) int {
	n := len(tools)
	for _, msg := range msgs {
		if msg != nil {
			n += len(msg.Content)
		}
	}
	return n
}

func compactionCall(id string, state message.ToolCallState) message.ToolCallBlock {
	return message.ToolCallBlock{Type: "tool_call", ID: id, Name: "Read", Input: `{"file_path":"` + id + `"}`, State: state}
}

func compactionResult(id string, state message.ToolResultState) message.ToolResultBlock {
	return message.ToolResultBlock{Type: "tool_result", ID: id, Name: "Read", Output: "observed " + id, State: state}
}

func compactionExchange(id string) []message.ContentBlock {
	return []message.ContentBlock{compactionCall(id, message.ToolCallFinished), compactionResult(id, message.ToolResultSuccess)}
}

func compactionMsg(blocks ...message.ContentBlock) *message.Msg {
	return message.AssistantMsg("bot", blocks)
}

func crossMessageCompactionHistory(prelude ...message.ContentBlock) []*message.Msg {
	return []*message.Msg{
		compactionMsg(compactionCall("old", message.ToolCallFinished)),
		message.NewMsg("tools", message.Role("tool"), []message.ContentBlock{compactionResult("old", message.ToolResultSuccess)}),
		compactionMsg(prelude...),
		compactionMsg(compactionCall("recent", message.ToolCallFinished)),
		message.NewMsg("tools", message.Role("tool"), []message.ContentBlock{compactionResult("recent", message.ToolResultSuccess)}),
	}
}

func TestContentBlockCompactionCrossMessagePrelude(t *testing.T) {
	prelude := []message.ContentBlock{
		message.ThinkingBlock{Type: "thinking", Thinking: "inspect recent", Extra: map[string]any{"signature": "fixture-signature"}},
		message.RedactedThinkingBlock{Type: "redacted_thinking", Data: "opaque-reasoning"},
		message.TextBlock{Type: "text", Text: "Inspect this observation."},
		message.DataBlock{Type: "data", Source: message.URLSource{Type: "url", URL: "https://example.invalid/chart.png", MediaType: "image/png"}},
	}
	for _, budget := range []int{6, 7} {
		t.Run(fmt.Sprintf("budget=%d", budget), func(t *testing.T) {
			msgs := crossMessageCompactionHistory(prelude...)
			before, err := json.Marshal(msgs)
			if err != nil {
				t.Fatal(err)
			}
			a := NewUnifiedAgent("bot", "system", &blockCountModel{}, WithContentBlockCompaction(), WithState(&AgentState{Context: msgs}))
			_, kept := a.splitContextForCompression(budget, nil)
			if budget == 6 {
				if len(kept) != 0 {
					t.Fatal("retained an exchange without its indivisible reasoning/text/media prelude")
				}
				return
			}
			if !reflect.DeepEqual(kept, msgs[2:]) {
				t.Fatalf("lost prelude order, signature or tool pair: %#v", kept)
			}
			after, err := json.Marshal(msgs)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("planning mutated the original cross-message history")
			}
		})
	}
}

func TestContentBlockCompactionCrossMessagePendingPrelude(t *testing.T) {
	thinking := message.ThinkingBlock{Type: "thinking", Thinking: "inspect recent", Extra: map[string]any{"signature": "fixture-signature"}}
	redacted := message.RedactedThinkingBlock{Type: "redacted_thinking", Data: "opaque-reasoning"}
	for _, state := range []message.ToolCallState{message.ToolCallPending, message.ToolCallAsking, message.ToolCallSubmitted} {
		t.Run(string(state), func(t *testing.T) {
			msgs := crossMessageCompactionHistory(thinking)[:3]
			msgs = append(msgs, nil, compactionMsg(), compactionMsg(redacted), compactionMsg(compactionCall("pending", state)))
			a := NewUnifiedAgent("bot", "system", &blockCountModel{}, WithContentBlockCompaction(), WithState(&AgentState{Context: msgs}))
			prefix, kept := a.splitContextForCompression(2, nil)
			if !reflect.DeepEqual(blockResultIDs(prefix), []string{"old"}) || !reflect.DeepEqual(kept, msgs[2:]) {
				t.Fatal("unfinished call did not pin its complete cross-message prelude above the reserve target")
			}
		})
	}
}

func TestContentBlockCompactionCrossMessagePreludeSeparators(t *testing.T) {
	thinking := message.ThinkingBlock{Type: "thinking", Thinking: "prior turn", Extra: map[string]any{"signature": "prior-signature"}}
	for _, separator := range []*message.Msg{message.UserMsg("user", "new task"), message.AssistantMsg("other", "handoff")} {
		t.Run(string(separator.Role)+"/"+separator.Name, func(t *testing.T) {
			msgs := crossMessageCompactionHistory(thinking)
			msgs = append(msgs[:3], append([]*message.Msg{separator}, msgs[3:]...)...)
			a := NewUnifiedAgent("bot", "system", &blockCountModel{}, WithContentBlockCompaction(), WithState(&AgentState{Context: msgs}))
			_, kept := a.splitContextForCompression(3, nil)
			if !reflect.DeepEqual(kept, msgs[4:]) {
				t.Fatal("retained a prior turn's prelude across a role or agent-name separator")
			}
		})
	}
}

func TestContentBlockCompactionCrossMessageParallelPrelude(t *testing.T) {
	thinking := message.ThinkingBlock{Type: "thinking", Thinking: "inspect both", Extra: map[string]any{"signature": "fixture-signature"}}
	msgs := crossMessageCompactionHistory(thinking)[:3]
	msgs = append(msgs, compactionMsg(compactionCall("a", message.ToolCallFinished)), compactionMsg(compactionCall("b", message.ToolCallSubmitted)), compactionMsg(compactionResult("a", message.ToolResultSuccess)))
	a := NewUnifiedAgent("bot", "system", &blockCountModel{}, WithContentBlockCompaction(), WithState(&AgentState{Context: msgs}))
	prefix, kept := a.splitContextForCompression(2, nil)
	if !reflect.DeepEqual(blockResultIDs(prefix), []string{"old"}) || !reflect.DeepEqual(kept, msgs[2:]) {
		t.Fatal("one terminal result released an overlapping unfinished group's prelude")
	}
}

func TestContentBlockCompactionBoundaries(t *testing.T) {
	old := compactionExchange("old")
	parallel := []message.ContentBlock{compactionCall("a", message.ToolCallFinished), compactionCall("b", message.ToolCallFinished), compactionResult("a", message.ToolResultSuccess), compactionResult("b", message.ToolResultSuccess)}
	type boundaryCase struct {
		name   string
		msgs   []*message.Msg
		budget int
		want   []string
		blocks int
	}
	cases := []boundaryCase{
		{"serial", []*message.Msg{compactionMsg(append(slices.Clone(old), compactionExchange("recent")...)...)}, 3, []string{"recent"}, 2},
		{"parallel fits", []*message.Msg{compactionMsg(append(slices.Clone(old), parallel...)...)}, 5, []string{"a", "b"}, 4},
		{"parallel indivisible", []*message.Msg{compactionMsg(append(slices.Clone(old), parallel...)...)}, 3, nil, 0},
		{"cross-message parallel", []*message.Msg{compactionMsg(old...), compactionMsg(parallel[0]), compactionMsg(parallel[1]), compactionMsg(parallel[2]), compactionMsg(parallel[3])}, 5, []string{"a", "b"}, 4},
		{"orphan dropped", []*message.Msg{compactionMsg(append(slices.Clone(old), compactionResult("missing", message.ToolResultSuccess))...), message.UserMsg("user", "recent")}, 3, nil, 1},
		{"running orphan dropped", []*message.Msg{compactionMsg(old...), compactionMsg(compactionResult("missing", message.ToolResultRunning)), message.UserMsg("user", "recent")}, 3, nil, 1},
		{"oversized exchange", []*message.Msg{compactionMsg(append(slices.Clone(old), compactionExchange("recent")...)...)}, 2, nil, 0},
		{"zero reserve", []*message.Msg{compactionMsg(append(slices.Clone(old), compactionExchange("recent")...)...)}, 0, nil, 0},
		{"other agent message indivisible", []*message.Msg{message.AssistantMsg("other", append(slices.Clone(old), compactionExchange("recent")...))}, 3, nil, 0},
		{"user message indivisible", []*message.Msg{message.UserMsg("user", []message.ContentBlock{message.TextBlock{Type: "text", Text: "one"}, message.TextBlock{Type: "text", Text: "two"}})}, 2, nil, 0},
		{"empty context", nil, 3, nil, 0},
		{"nil and empty message", []*message.Msg{nil, compactionMsg(), compactionMsg(old...)}, 3, []string{"old"}, 2},
		{"duplicate call IDs", []*message.Msg{compactionMsg(append(slices.Clone(old), compactionCall("old", message.ToolCallPending))...)}, 3, []string{"old"}, 3},
	}
	for _, state := range []message.ToolCallState{message.ToolCallPending, message.ToolCallAsking, message.ToolCallSubmitted, message.ToolCallFinished} {
		cases = append(cases, boundaryCase{
			"pin " + string(state), []*message.Msg{compactionMsg(append(slices.Clone(old), message.ThinkingBlock{Type: "thinking", Thinking: "next"}, compactionCall("pending", state))...)}, 2, nil, 2})
	}
	cases = append(cases, boundaryCase{
		"running is unfinished", []*message.Msg{compactionMsg(append(slices.Clone(old), compactionCall("pending", message.ToolCallFinished), compactionResult("pending", message.ToolResultRunning))...)}, 1, []string{"pending"}, 2})
	for _, state := range []message.ToolResultState{message.ToolResultError, message.ToolResultDenied, message.ToolResultInterrupted} {
		cases = append(cases, boundaryCase{"terminal " + string(state), []*message.Msg{compactionMsg(append(slices.Clone(old), compactionCall("recent", message.ToolCallFinished), compactionResult("recent", state))...)}, 3, []string{"recent"}, 2})
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			a := NewUnifiedAgent("bot", "system", &blockCountModel{}, WithContentBlockCompaction(), WithState(&AgentState{Context: tt.msgs}))
			before, err := json.Marshal(tt.msgs)
			if err != nil {
				t.Fatal(err)
			}
			compressed, kept := a.splitContextForCompression(tt.budget, nil)
			if got := blockResultIDs(kept); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("retained IDs=%v, want %v", got, tt.want)
			}
			n := 0
			for _, msg := range kept {
				if msg != nil {
					n += len(msg.Content)
				}
			}
			if n != tt.blocks {
				t.Fatalf("retained blocks=%d, want %d", n, tt.blocks)
			}
			after, err := json.Marshal(tt.msgs)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("planning mutated the input snapshot")
			}
			originalBlocks, plannedBlocks := 0, n
			for _, msg := range tt.msgs {
				if msg != nil {
					originalBlocks += len(msg.Content)
				}
			}
			for _, msg := range compressed {
				if msg != nil {
					plannedBlocks += len(msg.Content)
				}
			}
			if originalBlocks != plannedBlocks {
				t.Fatal("split lost or duplicated blocks")
			}
		})
	}
}

func TestContentBlockCompactionOwnsFragments(t *testing.T) {
	thinking := message.ThinkingBlock{Type: "thinking", ID: "reasoning", Thinking: "inspect latest", Extra: map[string]any{"signature": "opaque-signature"}}
	media := message.DataBlock{Type: "data", ID: "image", Name: "chart", Source: message.URLSource{Type: "url", URL: "https://example.invalid/chart.png", MediaType: "image/png"}}
	result := compactionResult("recent", message.ToolResultSuccess)
	result.Metadata = map[string]any{"source": "fixture"}
	original := compactionMsg(append(compactionExchange("old"), thinking, media, compactionCall("recent", message.ToolCallFinished), result)...)
	original.ID, original.Timestamp, original.FinishedAt = "logical-reply", "start", "finish"
	original.Metadata = types.JSONObject{"session": "one"}
	original.Usage = &message.Usage{InputTokens: 17, OutputTokens: 9, CacheInputTokens: 3, CacheCreationInputTokens: 4}
	before, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	a := NewUnifiedAgent("bot", "system", &blockCountModel{}, WithContentBlockCompaction(), WithState(&AgentState{ReplyID: original.ID, Context: []*message.Msg{original}}))
	compressed, kept := a.splitContextForCompression(5, nil)
	if len(compressed) != 1 || len(kept) != 1 || len(kept[0].Content) != 4 {
		t.Fatalf("unexpected fragments: compressed=%v kept=%v", compressed, kept)
	}
	if compressed[0].Usage != nil || !reflect.DeepEqual(kept[0].Usage, original.Usage) || kept[0].Usage == original.Usage {
		t.Fatal("usage was lost, duplicated, or shared")
	}
	if kept[0] == original || compressed[0] == original || kept[0].ID != original.ID || kept[0].Name != original.Name || kept[0].Role != original.Role || kept[0].Timestamp != original.Timestamp || kept[0].FinishedAt != original.FinishedAt || !reflect.DeepEqual(kept[0].Metadata, original.Metadata) {
		t.Fatal("message identity or metadata changed")
	}
	if !reflect.DeepEqual(kept[0].Content, original.Content[2:]) {
		t.Fatal("reasoning/media/tool metadata or state changed")
	}
	a.state.Context = kept
	a.saveToContext([]message.ContentBlock{message.TextBlock{Type: "text", Text: "continued"}}, &model.ChatUsage{InputTokens: 2, OutputTokens: 1})
	kept[0].Content[2] = compactionCall("recent", message.ToolCallPending)
	after, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("continuing the reply mutated the pre-compaction snapshot")
	}
	if kept[0].Usage.InputTokens != 19 || kept[0].Usage.OutputTokens != 10 || len(compressed[0].Content) != 2 {
		t.Fatal("usage accumulation or prefix ownership changed")
	}
}

type nonMonotonicCompactionModel struct{ blockCountModel }

func (*nonMonotonicCompactionModel) CountTokens(msgs []*message.Msg, _ []model.ToolSchema) int {
	switch len(blockResultIDs(msgs)) {
	case 0:
		return 1
	case 1:
		return 5
	case 2:
		return 4
	default:
		return 6
	}
}

func TestContentBlockCompactionNonMonotonicEstimate(t *testing.T) {
	blocks := append(compactionExchange("a"), compactionExchange("b")...)
	blocks = append(blocks, compactionExchange("c")...)
	a := NewUnifiedAgent("bot", "system", &nonMonotonicCompactionModel{}, WithContentBlockCompaction(), WithState(&AgentState{Context: []*message.Msg{compactionMsg(blocks...)}}))
	compressed, kept := a.splitContextForCompression(4, nil)
	if !reflect.DeepEqual(blockResultIDs(compressed), []string{"a"}) || !reflect.DeepEqual(blockResultIDs(kept), []string{"b", "c"}) {
		t.Fatalf("missed fitting group: compressed=%v kept=%v", blockResultIDs(compressed), blockResultIDs(kept))
	}
}
