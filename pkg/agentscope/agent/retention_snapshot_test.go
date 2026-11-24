package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func retentionConfig() RetentionPreviewConfig {
	return RetentionPreviewConfig{Limits: RetentionCopyLimits{MaxMessages: 100, MaxBlocks: 1000, MaxNodes: 10000, MaxPayloadBytes: 1 << 20, MaxAllocBytes: 4 << 20, MaxDepth: 32}, TTL: time.Minute}
}

func TestRetentionSnapshotOwnership(t *testing.T) {
	opt, err := WithRetentionPreview(retentionConfig())
	if err != nil {
		t.Fatal(err)
	}
	a := NewUnifiedAgent("a", "sys", &seamModel{}, opt)
	m := message.UserMsg("u", "hello")
	m.Metadata = types.JSONObject{"nested": map[string]any{"value": "before"}}
	if err := a.Observe(context.Background(), []*message.Msg{m}); err != nil {
		t.Fatal(err)
	}
	m.Metadata["nested"].(map[string]any)["value"] = "caller"
	h, err := a.CaptureRetentionSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	v, err := h.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.Messages[0].Metadata["nested"].(map[string]any)["value"] != "before" {
		t.Fatal("input aliases live context")
	}
	v.Messages[0].Metadata["nested"].(map[string]any)["value"] = "view"
	if err := h.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	v2, err := h.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v2.Messages[0].Metadata["nested"].(map[string]any)["value"] != "before" {
		t.Fatal("views share storage")
	}
}

func snapshotAgent(t *testing.T, opts ...AgentOption) *UnifiedAgent {
	t.Helper()
	o, err := WithRetentionPreview(retentionConfig())
	if err != nil {
		t.Fatal(err)
	}
	return NewUnifiedAgent("a", "sys", &seamModel{}, append([]AgentOption{o}, opts...)...)
}

func TestRetentionSnapshotLifecycle(t *testing.T) {
	a := snapshotAgent(t)
	ctx := context.Background()
	h, err := a.CaptureRetentionSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CaptureRetentionSnapshot(ctx); !errors.Is(err, ErrRetentionIneligible) {
		t.Fatal(err)
	}
	receipt, err := h.Finish(ctx)
	if err != nil || receipt.ValidatedAt.IsZero() {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if _, err := h.Finish(ctx); !errors.Is(err, ErrRetentionClosed) {
		t.Fatal(err)
	}
	h.Close()
	h.Close()
	next, err := a.CaptureRetentionSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	next.expires = time.Now().Add(-time.Second)
	a.mu.Unlock()
	if err := next.Validate(ctx); !errors.Is(err, ErrRetentionClosed) {
		t.Fatal(err)
	}
	last, err := a.CaptureRetentionSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	last.Close()
	var zero RetentionSnapshot
	if err := zero.Validate(ctx); !errors.Is(err, ErrRetentionClosed) {
		t.Fatal(err)
	}
	if _, err := zero.View(ctx); !errors.Is(err, ErrRetentionClosed) {
		t.Fatal(err)
	}
	if _, err := zero.Finish(ctx); !errors.Is(err, ErrRetentionClosed) {
		t.Fatal(err)
	}
	zero.Close()
}

func TestRetentionSnapshotStale(t *testing.T) {
	for _, mutate := range []string{"observe", "same-id", "metadata", "summary", "config", "ABA"} {
		t.Run(mutate, func(t *testing.T) {
			a := snapshotAgent(t)
			ctx := context.Background()
			if err := a.Observe(ctx, []*message.Msg{message.UserMsg("u", "original")}); err != nil {
				t.Fatal(err)
			}
			h, err := a.CaptureRetentionSnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			switch mutate {
			case "observe":
				err = a.Observe(ctx, []*message.Msg{message.UserMsg("u", "new")})
			case "same-id":
				a.state.Context[0].Content = []message.ContentBlock{message.TextBlock{Type: "text", Text: "edit"}}
			case "metadata":
				a.state.Context[0].Metadata = types.JSONObject{"changed": true}
			case "summary":
				a.state.Summary = "changed"
			case "config":
				a.systemPrompt = "changed"
			case "ABA":
				old := a.state.Context
				err = a.ApplyThreadUpdate(ctx, ThreadUpdate{Reset: true})
				if err == nil {
					err = a.Observe(ctx, old)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = h.Validate(ctx); !errors.Is(err, ErrRetentionStale) {
				t.Fatal(err)
			}
			if _, err = h.Finish(ctx); !errors.Is(err, ErrRetentionStale) {
				t.Fatal(err)
			}
			if _, err = a.CaptureRetentionSnapshot(ctx); err != nil {
				t.Fatal("failed Finish did not release slot", err)
			}
		})
	}
}

func TestRetentionSnapshotEligibility(t *testing.T) {
	for _, opts := range [][]AgentOption{
		{WithState(&AgentState{})},
		{WithContentBlockCompaction()},
		{WithContextConfig(&ContextConfig{PrecompressRatio: .5})},
		{WithContextConfig(&ContextConfig{CompactStrategy: CompactStrategyFreshWindow})},
	} {
		a := snapshotAgent(t, opts...)
		if _, err := a.CaptureRetentionSnapshot(context.Background()); !errors.Is(err, ErrRetentionIneligible) {
			t.Fatal(err)
		}
	}
	// WithState before the opt-in must also remain ineligible.
	o, _ := WithRetentionPreview(retentionConfig())
	a := NewUnifiedAgent("a", "sys", &seamModel{}, WithState(&AgentState{}), o)
	if _, err := a.CaptureRetentionSnapshot(context.Background()); !errors.Is(err, ErrRetentionIneligible) {
		t.Fatal(err)
	}
	b := NewUnifiedAgent("b", "sys", &seamModel{})
	if _, err := b.CaptureRetentionSnapshot(context.Background()); !errors.Is(err, ErrRetentionIneligible) {
		t.Fatal(err)
	}
}

func TestRetentionSnapshotCancellationAndLimits(t *testing.T) {
	cfg := retentionConfig()
	cfg.TTL = 0
	if _, err := WithRetentionPreview(cfg); err == nil {
		t.Fatal("accepted zero TTL")
	}
	cfg = retentionConfig()
	cfg.Limits.MaxNodes = 0
	if _, err := WithRetentionPreview(cfg); err == nil {
		t.Fatal("accepted invalid limits")
	}
	a := snapshotAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.CaptureRetentionSnapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	h, err := a.CaptureRetentionSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Finish(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	cfg = retentionConfig()
	cfg.Limits.MaxPayloadBytes = 8
	o, err := WithRetentionPreview(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := NewUnifiedAgent("b", "sys", &seamModel{}, o)
	m := message.UserMsg("u", strings.Repeat("x", 100))
	if err := b.Observe(context.Background(), []*message.Msg{m}); err == nil || len(b.state.Context) != 0 {
		t.Fatal("oversized ingress changed state", err)
	}
}

func TestRetentionSnapshotReadOnly(t *testing.T) {
	a := snapshotAgent(t)
	ctx := context.Background()
	if err := a.Observe(ctx, []*message.Msg{message.UserMsg("u", "hello")}); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(a.state)
	if err != nil {
		t.Fatal(err)
	}
	viewBefore := a.ThreadView()
	h, err := a.CaptureRetentionSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.View(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(a.state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !reflect.DeepEqual(viewBefore, a.ThreadView()) {
		t.Fatal("preview mutated checkpoint bytes or ThreadView")
	}
}

func TestRetentionReplyOutputOwnership(t *testing.T) {
	o, _ := WithRetentionPreview(retentionConfig())
	source := message.DataBlock{Type: "data", Source: message.Base64Source{Type: "base64", Data: "original"}}
	// Text + nested provider metadata exercise Reply and streamed/persisted output.
	block := message.TextBlock{Type: "text", Text: "answer"}
	m := &seamModel{respond: func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
		return &model.ChatResponse{Content: []message.ContentBlock{block}, IsLast: true}, nil
	}}
	a := NewUnifiedAgent("a", "sys", m, o)
	out, err := a.Reply(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	out.Content[0] = source
	if _, ok := a.state.Context[len(a.state.Context)-1].Content[0].(message.TextBlock); !ok {
		t.Fatal("Reply output aliases live message")
	}
}

func TestRetentionUnsupportedOutputInvalidates(t *testing.T) {
	a := snapshotAgent(t)
	a.saveToContext([]message.ContentBlock{message.ToolResultBlock{Type: "tool_result", Output: make(chan int)}}, nil)
	if _, err := a.CaptureRetentionSnapshot(context.Background()); !errors.Is(err, ErrRetentionIneligible) {
		t.Fatal(err)
	}
}

func TestRetentionSnapshotRejectsInflightCompaction(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	m := &seamModel{tokens: func([]*message.Msg) int { return 1 }, respond: func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
		close(entered)
		<-release
		return strictSummaryResponse(), nil
	}}
	o, _ := WithRetentionPreview(retentionConfig())
	a := strictAgent(m)
	o(a)
	h, err := a.CaptureRetentionSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	done := make(chan error, 1)
	go func() { _, err := a.CompactStrict(context.Background(), CompactStrictOptions{}); done <- err }()
	<-entered
	_, captureErr := a.CaptureRetentionSnapshot(context.Background())
	validateErr := h.Validate(context.Background())
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(captureErr, ErrRetentionIneligible) || !errors.Is(validateErr, ErrRetentionStale) {
		t.Fatalf("capture=%v validate=%v", captureErr, validateErr)
	}
}

func TestRetentionStoredOutputIndependence(t *testing.T) {
	a := snapshotAgent(t)
	raw := []byte("original")
	output := map[string]any{"bytes": raw}
	blocks := []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: "call", Output: output}}
	a.saveToContext(blocks, nil)
	raw[0] = 'X'
	output["new"] = true
	h, err := a.CaptureRetentionSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	view, err := h.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	copied := view.Messages[0].Content[0].(message.ToolResultBlock).Output.(map[string]any)
	if string(copied["bytes"].([]byte)) != "original" || copied["new"] != nil {
		t.Fatal("persisted output shares event/provider storage")
	}
}

func TestRetentionThreadIngressIndependence(t *testing.T) {
	a := snapshotAgent(t)
	m := message.UserMsg("u", "original")
	if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{Messages: []*message.Msg{m}}); err != nil {
		t.Fatal(err)
	}
	m.Content[0] = message.TextBlock{Type: "text", Text: "caller edit"}
	h, err := a.CaptureRetentionSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	v, err := h.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.Messages[0].Content[0].(message.TextBlock).Text != "original" {
		t.Fatal("thread ingress aliases caller")
	}
}

func BenchmarkRetentionCapture(b *testing.B) {
	cfg := retentionConfig()
	o, _ := WithRetentionPreview(cfg)
	a := NewUnifiedAgent("a", "sys", &seamModel{}, o)
	if err := a.Observe(context.Background(), []*message.Msg{message.UserMsg("u", strings.Repeat("x", 1<<18))}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h, err := a.CaptureRetentionSnapshot(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		h.Close()
	}
}

func TestRetentionStreamEventIndependence(t *testing.T) {
	o, _ := WithRetentionPreview(retentionConfig())
	extra := map[string]any{"nested": map[string]any{"value": "original"}}
	m := &seamModel{respond: func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		if call > 0 {
			return seamText("done"), nil
		}
		return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "external", Name: "external_tool", Input: `{}`, State: message.ToolCallPending, Extra: extra}}, IsLast: true}, nil
	}}
	a := NewUnifiedAgent("a", "sys", m, o, WithToolkit(tool.NewToolkit(newHITLMockTool("external_tool", true))), WithPermissionContext(permission.NewContext(permission.ModeBypass)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := a.ReplyStream(ctx, "call external")
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for e := range stream {
		if request, ok := e.(event.RequireExternalExecutionEvent); ok {
			seen = true
			request.ToolCalls[0].Extra["nested"].(map[string]any)["value"] = "event edit"
			a.SubmitExternalResult(&event.ExternalExecutionResultEvent{ExecutionResults: []message.ToolResultBlock{{Type: "tool_result", ID: "external", State: message.ToolResultSuccess, Output: "answer"}}})
		}
	}
	if !seen {
		t.Fatal("no external request")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, msg := range a.state.Context {
		for _, block := range msg.Content {
			if tc, ok := block.(message.ToolCallBlock); ok && tc.ID == "external" {
				if tc.Extra["nested"].(map[string]any)["value"] != "original" {
					t.Fatal("event payload aliases context")
				}
				return
			}
		}
	}
	t.Fatal("missing persisted call")
}

func TestRetentionSnapshotPlanAndLazyExpiry(t *testing.T) {
	a := snapshotAgent(t, WithContextConfig(&ContextConfig{}))
	a.state.Plan = []tool.PlanItem{{Step: "first", Status: "pending"}}
	h, err := a.CaptureRetentionSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	v, err := h.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	v.Plan[0].Step = "edited"
	if err = h.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	h.expires = time.Now().Add(-time.Second)
	a.mu.Unlock()
	next, err := a.CaptureRetentionSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if _, err = h.View(context.Background()); !errors.Is(err, ErrRetentionClosed) {
		t.Fatal(err)
	}
}

func TestRetentionSnapshotCopyFailures(t *testing.T) {
	a := snapshotAgent(t)
	m := message.UserMsg("u", "text")
	m.Metadata = types.JSONObject{"bad": make(chan int)}
	if err := a.Observe(context.Background(), []*message.Msg{m}); err == nil {
		t.Fatal("accepted unsupported ingress")
	}
	if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{Messages: []*message.Msg{m}}); err == nil {
		t.Fatal("accepted unsupported thread ingress")
	}
	h, err := a.CaptureRetentionSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a.state.Context = []*message.Msg{m}
	if err = h.Validate(context.Background()); err == nil {
		t.Fatal("accepted uncopyable context")
	}
	if _, err = h.View(context.Background()); err == nil {
		t.Fatal("returned invalid view")
	}
	h.Close()
	if _, err = a.CaptureRetentionSnapshot(context.Background()); err == nil {
		t.Fatal("captured uncopyable context")
	}
}
