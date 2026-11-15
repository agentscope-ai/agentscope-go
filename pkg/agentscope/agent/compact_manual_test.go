package agent

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestCompactBelowThresholdCompacts(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 5000, contextSize: 32000}
	a := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8, ReserveRatio: 0.1}),
	)
	for i := 0; i < 10; i++ {
		a.state.Context = append(a.state.Context,
			message.UserMsg("user", "message"),
			message.AssistantMsg("test", "reply"),
		)
	}

	// Control: the automatic path stays quiet below the trigger ratio.
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&mock.chatCallCount) != 0 {
		t.Fatal("automatic compression fired below the threshold")
	}

	compressed, err := a.Compact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !compressed {
		t.Fatal("Compact reported no change")
	}
	if got := atomic.LoadInt64(&mock.chatCallCount); got != 1 {
		t.Fatalf("chat calls = %d, want 1", got)
	}
	if !strings.Contains(a.state.Summary, "Test task") {
		t.Fatalf("summary = %q", a.state.Summary)
	}
	if len(a.state.Context) >= 20 {
		t.Fatalf("context not reduced: %d messages", len(a.state.Context))
	}
}

func TestCompactRequiresContextConfig(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 100, contextSize: 32000}
	a := NewUnifiedAgent("test", "prompt", mock)

	compressed, err := a.Compact(context.Background())
	if err == nil {
		t.Fatal("Compact without WithContextConfig should fail")
	}
	if compressed {
		t.Fatal("compressed = true without configuration")
	}
	if atomic.LoadInt64(&mock.chatCallCount) != 0 {
		t.Fatal("model called without configuration")
	}
}

func TestCompactEmptyContextNoOp(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 100, contextSize: 32000}
	a := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8}),
	)

	compressed, err := a.Compact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if compressed {
		t.Fatal("compressed = true over an empty context")
	}
	if atomic.LoadInt64(&mock.chatCallCount) != 0 {
		t.Fatal("model called over an empty context")
	}
}

func TestCompactCanceledContext(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 100, contextSize: 32000}
	a := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Compact(ctx); err == nil {
		t.Fatal("Compact with a canceled context should fail")
	}
	if atomic.LoadInt64(&mock.chatCallCount) != 0 {
		t.Fatal("model called with a canceled context")
	}
}

// A middleware that raises the trigger ratio must not defeat a manual
// Compact: force is a flag precisely because middleware can rewrite the
// ratio it passes down the chain.
type ratioRaisingMiddleware struct {
	middleware.BaseMiddleware
}

func (m *ratioRaisingMiddleware) OnCompressContext(
	ctx context.Context,
	input middleware.CompressInput,
	next middleware.CompressHandler,
) error {
	input.TriggerRatio = 0.9
	return next(ctx, input)
}

func TestCompactForceBeatsMiddlewareRatio(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 60, contextSize: 100, chatResponse: summaryToolCallResponse(nil)}
	a := NewUnifiedAgent("t", "sys", mock,
		WithContextConfig(&ContextConfig{ContextSize: 100, TriggerRatio: 0.8, ReserveRatio: 0.1}),
		WithMiddlewares(&ratioRaisingMiddleware{}),
	)
	for i := 0; i < 6; i++ {
		a.state.Context = append(a.state.Context, textMsg("history"))
	}

	// Control: 60 < 90 (the middleware-raised threshold), so the automatic
	// path stays quiet.
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&mock.chatCallCount) != 0 {
		t.Fatal("automatic compression fired below the raised threshold")
	}

	compressed, err := a.Compact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !compressed {
		t.Fatal("force did not bypass the middleware-raised threshold")
	}
}

func TestCompactWithContentBlockCompaction(t *testing.T) {
	msgs := []*message.Msg{compactionMsg(append(compactionExchange("old"), compactionExchange("recent")...)...)}
	a := NewUnifiedAgent("bot", "system", &blockCountModel{},
		WithContentBlockCompaction(),
		WithContextConfig(&ContextConfig{ContextSize: 10, TriggerRatio: 0.9, ReserveRatio: 0.4}),
		WithState(&AgentState{Context: msgs}),
	)

	// Control: 5 blocks (system 1 + history 4) against a threshold of 9, so
	// the automatic path stays quiet.
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.state.Summary != "" {
		t.Fatal("automatic compression fired below the threshold")
	}

	compressed, err := a.Compact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !compressed {
		t.Fatal("Compact reported no change")
	}
	if got := blockResultIDs(a.state.Context); !reflect.DeepEqual(got, []string{"recent"}) {
		t.Fatalf("retained results = %v, want [recent]", got)
	}
	if a.state.Summary == "" {
		t.Fatal("summary not set")
	}
}

// compactFlowModel answers ordinary chat with a short text and compression
// calls with a structured summary, recording the message texts it saw.
type compactFlowModel struct {
	compressionMockModel

	mu     sync.Mutex
	rounds [][]string
}

func (m *compactFlowModel) Chat(_ context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	var cfg model.CallOptions
	for _, opt := range opts {
		opt(&cfg)
	}
	if len(cfg.Tools) == 1 && cfg.Tools[0].Function.Name == "generate_structured_output" {
		return &model.ChatResponse{
			IsLast: true,
			Content: []message.ContentBlock{message.ToolCallBlock{
				Type:  "tool_call",
				ID:    "tc_summary",
				Name:  "generate_structured_output",
				Input: `{"task_overview":"manual-compact-marker","current_state":"S","important_discoveries":"D","next_steps":"N","context_to_preserve":"P"}`,
				State: message.ToolCallPending,
			}},
		}, nil
	}
	var texts []string
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		for _, block := range msg.Content {
			if tb, ok := block.(message.TextBlock); ok {
				texts = append(texts, tb.Text)
			}
		}
	}
	m.mu.Lock()
	m.rounds = append(m.rounds, texts)
	m.mu.Unlock()
	return &model.ChatResponse{
		IsLast:  true,
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}},
	}, nil
}

// The host-UI flow: a reply, a manual compact, then the next reply — which
// must see the manual summary.
func TestCompactBetweenReplies(t *testing.T) {
	mock := &compactFlowModel{compressionMockModel: compressionMockModel{tokenCount: 5000, contextSize: 32000}}
	a := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8, ReserveRatio: 0.1}),
	)

	if _, err := a.Reply(context.Background(), "first task"); err != nil {
		t.Fatal(err)
	}
	compressed, err := a.Compact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !compressed {
		t.Fatal("Compact reported no change")
	}
	if _, err := a.Reply(context.Background(), "second task"); err != nil {
		t.Fatal(err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.rounds) != 2 {
		t.Fatalf("chat rounds = %d, want 2", len(mock.rounds))
	}
	found := false
	for _, text := range mock.rounds[1] {
		if strings.Contains(text, "manual-compact-marker") {
			found = true
		}
	}
	if !found {
		t.Fatalf("second reply did not receive the manual summary: %v", mock.rounds[1])
	}
}
