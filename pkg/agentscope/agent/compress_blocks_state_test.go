package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type blockStateModel struct {
	blockCountModel
	chat   func(context.Context) (*model.ChatResponse, error)
	inputs [][]string
}

func (m *blockStateModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	m.inputs = append(m.inputs, blockResultIDs(msgs))
	if m.chat != nil {
		return m.chat(ctx)
	}
	return m.blockCountModel.Chat(ctx, msgs, opts...)
}

type cancelCompactionOffloader struct {
	fakeOffloader
	cancel context.CancelFunc
}

func (o *cancelCompactionOffloader) OffloadContent(ctx context.Context, content, filename string) (string, error) {
	o.cancel()
	return o.fakeOffloader.OffloadContent(ctx, content, filename)
}

func TestContentBlockCompactionFailureAndCancellation(t *testing.T) {
	for _, phase := range []string{"generation failure", "format failure", "cancel before", "cancel after success", "cancel after failure", "cancel while offloading success", "cancel while offloading fallback"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m := &blockStateModel{}
			response := summaryToolCallResponse(&model.ChatUsage{InputTokens: 11, OutputTokens: 7})
			m.chat = func(context.Context) (*model.ChatResponse, error) {
				if phase == "cancel after success" || phase == "cancel after failure" {
					cancel()
				}
				if phase == "generation failure" || phase == "cancel after failure" || phase == "cancel while offloading fallback" {
					return nil, errors.New("fixture summary failure")
				}
				if phase == "format failure" {
					call := response.Content[0].(message.ToolCallBlock)
					call.Input = `{"task_overview":42}`
					response.Content[0] = call
				}
				return response, nil
			}
			original := compactionMsg(append(compactionExchange("old"), compactionExchange("recent")...)...)
			state := &AgentState{Context: []*message.Msg{original}, Summary: "previous summary"}
			cache := tool.NewReadCache(0, 0)
			mtime := time.Unix(1, 0)
			for _, path := range []string{"old", "recent"} {
				cache.CacheFileWithMtime(path, []string{"fixture"}, &mtime)
			}
			opts := []AgentOption{WithState(state), WithReadCache(cache), WithContentBlockCompaction(), WithContextConfig(&ContextConfig{ContextSize: 10, TriggerRatio: .5, ReserveRatio: .4})}
			if phase == "cancel while offloading success" || phase == "cancel while offloading fallback" {
				opts = append(opts, WithOffloader(&cancelCompactionOffloader{cancel: cancel}))
			}
			a := NewUnifiedAgent("bot", "system", m, opts...)
			before, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "cancel before" {
				cancel()
			}
			err = a.compressContext(ctx)
			if ctx.Err() != nil {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel error=%v", err)
				}
				after, marshalErr := json.Marshal(state)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if !bytes.Equal(before, after) || a.compactionVersion != 0 || cache.Len() != 2 {
					t.Fatal("canceled plan published state or cleaned the cache")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if state.Summary != "previous summary" || !reflect.DeepEqual(blockResultIDs(state.Context), []string{"recent"}) {
					t.Fatal("failure did not atomically retain the previous summary and recent exchange")
				}
				if cache.HasBeenRead("old") || !cache.HasBeenRead("recent") {
					t.Fatal("read-cache cleanup does not match retained Read calls")
				}
			}
			if len(original.Content) != 4 {
				t.Fatal("original message was partially mutated")
			}
			if phase == "cancel after success" {
				if usage := a.takeCompressionUsage(); usage == nil || usage.InputTokens != 11 || usage.OutputTokens != 7 {
					t.Fatal("canceled summary lost incurred usage")
				}
			}
		})
	}
}

func TestContentBlockCompactionRepeatedSummaryReportsChange(t *testing.T) {
	response := summaryToolCallResponse(&model.ChatUsage{InputTokens: 11, OutputTokens: 7})
	summary, err := formatSummary(defaultSummaryTemplate, json.RawMessage(response.Content[0].(message.ToolCallBlock).Input))
	if err != nil {
		t.Fatal(err)
	}
	m := &blockStateModel{}
	m.chatResponse = response
	a := NewUnifiedAgent("bot", "system", m, WithContentBlockCompaction(), WithContextConfig(&ContextConfig{ContextSize: 10, TriggerRatio: .8, ReserveRatio: .4, AgentDrivenTriggerRatio: .5}), WithState(&AgentState{Summary: summary, Context: []*message.Msg{compactionMsg(append(compactionExchange("old"), compactionExchange("recent")...)...)}}))
	result, err := a.compressContextForTool(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Compressed || a.state.Summary != summary || len(a.state.Context) != 1 || len(a.state.Context[0].Content) != 2 {
		t.Fatal("same-summary content compaction reported a no-op")
	}
	version := a.compactionVersion
	result, err = a.compressContextForTool(context.Background())
	if err != nil || result.Compressed || a.compactionVersion != version {
		t.Fatalf("below-threshold call changed state: result=%+v err=%v", result, err)
	}
	if usage := a.takeCompressionUsage(); usage == nil || usage.InputTokens != 11 || usage.OutputTokens != 7 || a.takeCompressionUsage() != nil {
		t.Fatal("compression usage was lost or duplicated")
	}
}

func TestContentBlockCompactionWholesaleFallback(t *testing.T) {
	for _, blocks := range [][]message.ContentBlock{
		{compactionCall("pending", message.ToolCallSubmitted)},
		compactionExchange("oversized"),
	} {
		a := NewUnifiedAgent("bot", "system", &blockCountModel{}, WithContentBlockCompaction(), WithContextConfig(&ContextConfig{ContextSize: 10, TriggerRatio: .1, ReserveRatio: .1}), WithState(&AgentState{Context: []*message.Msg{compactionMsg(blocks...)}}))
		if err := a.compressContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(a.state.Context) != 0 || a.state.Summary == "" {
			t.Fatal("indivisible/no-prefix input did not use the wholesale emergency fallback")
		}
	}
}

func TestContentBlockCompactionInFlightCompressionTool(t *testing.T) {
	a := NewUnifiedAgent("bot", "system", &blockCountModel{}, WithContentBlockCompaction(), WithAgentDrivenCompression(), WithContextConfig(&ContextConfig{ContextSize: 20, TriggerRatio: .8, ReserveRatio: .3, AgentDrivenTriggerRatio: .4}))
	blocks := append(compactionExchange("one"), compactionExchange("two")...)
	blocks = append(blocks, compactionExchange("three")...)
	pending := compactionCall("compact", message.ToolCallAllowed)
	pending.Name = "compress_context"
	blocks = append(blocks, pending)
	a.state.Context = []*message.Msg{compactionMsg(blocks...)}
	if _, err := a.toolkit.CallTool(context.Background(), "compress_context", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if a.state.Summary == "" {
		t.Fatal("compression tool did not run")
	}
	if !reflect.DeepEqual(blockResultIDs(a.state.Context), []string{"three"}) {
		t.Fatalf("retained IDs=%v", blockResultIDs(a.state.Context))
	}
	last := a.state.Context[0]
	if len(last.Content) != 3 || last.Content[2].(message.ToolCallBlock).ID != "compact" {
		t.Fatal("in-flight compress_context was summarized away")
	}
	a.saveToContext([]message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: "compact", Name: "compress_context", Output: "compressed", State: message.ToolResultSuccess}}, nil)
	if !reflect.DeepEqual(blockResultIDs(a.state.Context), []string{"three", "compact"}) {
		t.Fatal("compression result lost its call")
	}
}

func TestContentBlockCompactionLegacyCheckpoint(t *testing.T) {
	saver := newFakeStateSaver()
	state := &AgentState{SessionID: "legacy-session", ReplyID: "logical-reply", Context: []*message.Msg{compactionMsg(append(compactionExchange("old"), compactionExchange("recent")...)...)}}
	state.Context[0].ID = state.ReplyID
	a := NewUnifiedAgent("bot", "system", &blockCountModel{}, WithState(state), WithStateSaver(saver), WithContentBlockCompaction(), WithContextConfig(&ContextConfig{ContextSize: 10, TriggerRatio: .5, ReserveRatio: .3}))
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveCheckpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCheckpoint(context.Background(), saver, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SchemaVersion != 1 || loaded.ReplyID != state.ReplyID || loaded.Summary != state.Summary || !reflect.DeepEqual(blockResultIDs(loaded.Context), []string{"recent"}) {
		t.Fatalf("checkpoint changed compacted state: %+v", loaded)
	}
	m := &blockStateModel{chat: func(context.Context) (*model.ChatResponse, error) {
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "continued"}}, IsLast: true}, nil
	}}
	// A large window avoids a second compaction and isolates legacy restore.
	restored := NewUnifiedAgent("bot", "system", m, WithState(loaded), WithModelStreaming(false), WithContentBlockCompaction(), WithContextConfig(&ContextConfig{ContextSize: 1000}))
	if _, err := restored.Reply(context.Background(), "continue"); err != nil {
		t.Fatal(err)
	}
	if len(m.inputs) != 1 || !reflect.DeepEqual(m.inputs[0], []string{"recent"}) {
		t.Fatalf("restored model input=%v", m.inputs)
	}
}

func TestContentBlockCompactionReplyRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var checkpoint []byte
	saver := &recoveryHookSaver{fakeStateSaver: newFakeStateSaver(), hook: func(state *AgentState) error {
		if state.Summary == "" || checkpoint != nil {
			return nil
		}
		for _, msg := range state.Context {
			for _, block := range msg.Content {
				if call, ok := block.(message.ToolCallBlock); ok && call.ID == "read-4" {
					var err error
					checkpoint, err = json.Marshal(state)
					if err != nil {
						return err
					}
					// Stop at the existing pre-acting checkpoint, after recording
					// its bytes, to model a process loss before the pending tool.
					return errors.New("fixture stops after recording checkpoint")
				}
			}
		}
		return nil
	}}
	counter, err := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "fixture", Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	executions := 0
	read := tool.NewFunctionTool("read_log", "Read one diagnostic log", json.RawMessage(`{"type":"object","properties":{}}`), func(context.Context, map[string]any) (any, error) {
		executions++
		return strings.Repeat("INFO request processed latency_ms=23 status=200\n", 90), nil
	})
	options := []AgentOption{WithToolkit(tool.NewToolkit(read)), WithModelStreaming(false), WithContentBlockCompaction(), WithReplyRecovery(), WithStateSaver(saver), WithReactConfig(ReactConfig{MaxIters: 12}), WithModelConfig(ModelConfig{MaxRetries: 1}), WithContextConfig(&ContextConfig{ContextSize: 4096, TriggerRatio: .8, ReserveRatio: .3, ToolResultLimit: 1200})}
	a := NewUnifiedAgent("analyst", "Analyze diagnostic logs.", &blockCompactionFixture{counter: counter}, options...)
	stream, err := a.ReplyStream(ctx, "Inspect four logs, then report findings.")
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if len(checkpoint) == 0 || executions != 3 {
		t.Fatalf("missing compacted checkpoint; executions=%d", executions)
	}
	var saved AgentState
	if err := json.Unmarshal(checkpoint, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.SchemaVersion != 2 || saved.ReplyRecovery == nil || !saved.ReplyRecovery.Active || !reflect.DeepEqual(blockResultIDs(saved.Context), []string{"read-3"}) {
		t.Fatalf("checkpoint does not hold compacted active reply: schema=%d recovery=%+v ids=%v", saved.SchemaVersion, saved.ReplyRecovery, blockResultIDs(saved.Context))
	}
	replyID := saved.ReplyID
	resumedFixture := &blockCompactionFixture{counter: counter, rounds: 4}
	resumed := NewUnifiedAgent("analyst", "Analyze diagnostic logs.", resumedFixture, append(options, WithState(&saved), WithStateSaver(newFakeStateSaver()))...)
	stream, err = resumed.ResumeReplyStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for ev := range stream {
		if end, ok := ev.(event.ReplyEndEvent); ok && end.Error != nil {
			t.Fatal(end.Error)
		}
	}
	if executions != 4 || resumed.state.ReplyID != replyID || !reflect.DeepEqual(blockResultIDs(resumed.state.Context), []string{"read-3", "read-4"}) {
		t.Fatalf("resume re-executed tools or changed context: executions=%d ids=%v", executions, blockResultIDs(resumed.state.Context))
	}
	if len(resumedFixture.inputs) != 1 || !reflect.DeepEqual(resumedFixture.inputs[0], []string{"read-3", "read-4"}) {
		t.Fatalf("resumed reasoning inputs=%v", resumedFixture.inputs)
	}
	if resumed.state.ReplyRecovery.Active {
		t.Fatal("resumed reply did not finish")
	}
}
