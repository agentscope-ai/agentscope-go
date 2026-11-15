package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

const strictSummaryJSON = `{"task_overview":"strict task","current_state":"done","important_discoveries":"none","next_steps":"continue","context_to_preserve":"nothing"}`

func strictSummaryResponse() *model.ChatResponse {
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.ToolCallBlock{
			Type: "tool_call", ID: "s", Name: "generate_structured_output", Input: strictSummaryJSON,
		}},
		IsLast: true,
		Usage:  &model.ChatUsage{InputTokens: 40, OutputTokens: 9, CacheInputTokens: 30},
	}
}

func strictAgent(m *seamModel) *UnifiedAgent {
	a := NewUnifiedAgent("a", "sys", m, WithContextConfig(&ContextConfig{ContextSize: 1000}))
	for i := 0; i < 6; i++ {
		a.state.Context = append(a.state.Context, message.UserMsg("u", strings.Repeat("x", 50)))
	}
	a.state.Summary = "old summary"
	return a
}

func TestCompactStrictSuccess(t *testing.T) {
	var prompt string
	m := &seamModel{tokens: func([]*message.Msg) int { return 1 }}
	m.respond = func(_ int, msgs []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		prompt = msgText(msgs[len(msgs)-1])
		return strictSummaryResponse(), nil
	}
	a := strictAgent(m)
	changed, err := a.CompactStrict(context.Background(), CompactStrictOptions{Prompt: "Summarize strictly."})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !strings.Contains(a.state.Summary, "strict task") || len(a.state.Context) != 0 {
		t.Fatalf("summary=%q context=%d", a.state.Summary, len(a.state.Context))
	}
	if prompt != "Summarize strictly." {
		t.Fatalf("the custom prompt must replace the configured one, got %q", prompt)
	}
	if u := a.takeCompressionUsage(); u == nil || u.InputTokens != 40 || u.CacheInputTokens != 30 {
		t.Fatalf("compression usage must be recorded: %+v", u)
	}
}

func TestCompactStrictPersists(t *testing.T) {
	respond := func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
		return strictSummaryResponse(), nil
	}
	saver := newFakeStateSaver()
	a := strictAgent(&seamModel{tokens: func([]*message.Msg) int { return 1 }, respond: respond})
	a.stateSaver = saver
	if changed, err := a.CompactStrict(context.Background(), CompactStrictOptions{}); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	saved := saver.states[a.state.SessionID]
	if saved == nil || !strings.Contains(saved.Summary, "strict task") || len(saved.Context) != 0 {
		t.Fatalf("the compacted state was not saved: %+v", saved)
	}

	// Nothing to compact: nothing changed, nothing saved.
	delete(saver.states, a.state.SessionID)
	if changed, err := a.CompactStrict(context.Background(), CompactStrictOptions{}); err != nil || changed {
		t.Fatalf("empty context: changed=%v err=%v", changed, err)
	}
	if saver.states[a.state.SessionID] != nil {
		t.Fatal("an unchanged state must not be saved")
	}

	// A failed save is reported; the compaction itself stands.
	b := strictAgent(&seamModel{tokens: func([]*message.Msg) int { return 1 }, respond: respond})
	b.stateSaver = &errSaver{fakeStateSaver: newFakeStateSaver(), err: errors.New("disk full")}
	changed, err := b.CompactStrict(context.Background(), CompactStrictOptions{})
	if !changed || err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("a failed save: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(b.state.Summary, "strict task") {
		t.Fatal("the compaction must stay applied in memory")
	}
}

func TestCompactStrictLeavesStateOnError(t *testing.T) {
	for name, respond := range map[string]func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error){
		"generation error": func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
			return nil, errors.New("HTTP 400 summary failed")
		},
		"unformattable summary": func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
			r := strictSummaryResponse()
			r.Content = []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "s", Name: "generate_structured_output", Input: `{"task_overview":7}`}}
			return r, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := &seamModel{tokens: func([]*message.Msg) int { return 1 }, respond: respond}
			a := strictAgent(m)
			before := append([]*message.Msg(nil), a.state.Context...)
			changed, err := a.CompactStrict(context.Background(), CompactStrictOptions{})
			if err == nil || changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if a.state.Summary != "old summary" || !reflect.DeepEqual(before, a.state.Context) {
				t.Fatal("a failed strict compaction must leave the state exactly as before")
			}
			for i := range before {
				if a.state.Context[i] != before[i] {
					t.Fatal("context messages were replaced")
				}
			}
		})
	}

	// A custom template over blank fields formats to a blank summary, which
	// would replace the history with nothing.
	t.Run("blank summary", func(t *testing.T) {
		m := &seamModel{tokens: func([]*message.Msg) int { return 1 }, respond: func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
			r := strictSummaryResponse()
			r.Content = []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "s", Name: "generate_structured_output", Input: `{"task_overview":"  "}`}}
			return r, nil
		}}
		a := NewUnifiedAgent("a", "sys", m, WithContextConfig(&ContextConfig{ContextSize: 1000, SummaryTemplate: "{task_overview}"}))
		for i := 0; i < 6; i++ {
			a.state.Context = append(a.state.Context, message.UserMsg("u", strings.Repeat("x", 50)))
		}
		a.state.Summary = "old summary"
		before := append([]*message.Msg(nil), a.state.Context...)
		changed, err := a.CompactStrict(context.Background(), CompactStrictOptions{})
		if err == nil || changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		if a.state.Summary != "old summary" || !reflect.DeepEqual(before, a.state.Context) {
			t.Fatal("a blank summary must leave the state exactly as before")
		}
	})

	// Contrast: Compact degrades to truncation on the same failure.
	m := &seamModel{tokens: func([]*message.Msg) int { return 1 }, respond: func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
		return nil, errors.New("HTTP 400 summary failed")
	}}
	a := strictAgent(m)
	if changed, err := a.Compact(context.Background()); err != nil || !changed {
		t.Fatalf("Compact is expected to fall back: changed=%v err=%v", changed, err)
	}
}

func TestCompactStrictLeavesStateOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &seamModel{tokens: func([]*message.Msg) int { return 1 }}
	m.respond = func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
		cancel()
		return strictSummaryResponse(), nil // a response that arrives after cancellation is discarded
	}
	a := strictAgent(m)
	before := append([]*message.Msg(nil), a.state.Context...)
	changed, err := a.CompactStrict(ctx, CompactStrictOptions{})
	if !errors.Is(err, context.Canceled) || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if a.state.Summary != "old summary" || !reflect.DeepEqual(before, a.state.Context) {
		t.Fatal("a canceled strict compaction must leave the state exactly as before")
	}

	done, stop := context.WithCancel(context.Background())
	stop()
	if _, err := a.CompactStrict(done, CompactStrictOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("an already canceled context: %v", err)
	}
}

// TestCompactStrictRefusesChangedContext: the split is planned before the
// summary is generated; a change made meanwhile (another between-replies
// call) must fail the compaction and survive, not be overwritten.
func TestCompactStrictRefusesChangedContext(t *testing.T) {
	for name, change := range map[string]func(*UnifiedAgent) error{
		"message appended": func(a *UnifiedAgent) error {
			return a.ApplyThreadUpdate(context.Background(), ThreadUpdate{Messages: []*message.Msg{importedMsg(9, "late entry")}})
		},
		"summary changed": func(a *UnifiedAgent) error {
			a.mu.Lock()
			a.state.Summary = "changed meanwhile"
			a.mu.Unlock()
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			var a *UnifiedAgent
			m := &seamModel{tokens: func([]*message.Msg) int { return 1 }}
			m.respond = func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
				if err := change(a); err != nil {
					t.Error(err)
				}
				return strictSummaryResponse(), nil
			}
			a = strictAgent(m)
			before := append([]*message.Msg(nil), a.state.Context...)
			_, err := a.CompactStrict(context.Background(), CompactStrictOptions{})
			if !errors.Is(err, errStrictStale) {
				t.Fatalf("err = %v, want the stale-context error", err)
			}
			if strings.Contains(a.state.Summary, "strict task") || len(a.state.Context) < len(before) {
				t.Fatalf("a stale summary must not be committed: summary=%q context=%d", a.state.Summary, len(a.state.Context))
			}
			for i := range before {
				if a.state.Context[i] != before[i] {
					t.Fatal("context messages were replaced")
				}
			}
		})
	}
}

func TestCompactStrictPreconditions(t *testing.T) {
	m := &seamModel{respond: func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
		return strictSummaryResponse(), nil
	}}
	plain := NewUnifiedAgent("a", "sys", m)
	if _, err := plain.CompactStrict(context.Background(), CompactStrictOptions{}); err == nil {
		t.Fatal("CompactStrict requires WithContextConfig")
	}

	// A parked call must not be folded into the summary by the reserve-zero split.
	a := strictAgent(&seamModel{tokens: func([]*message.Msg) int { return 1 }, respond: m.respond})
	a.state.Context = append(a.state.Context, message.AssistantMsg("a", []message.ContentBlock{
		message.ToolCallBlock{Type: "tool_call", ID: "p", Name: "echo", Input: `{}`, State: message.ToolCallAsking},
	}))
	before := len(a.state.Context)
	if _, err := a.CompactStrict(context.Background(), CompactStrictOptions{}); err == nil || len(a.state.Context) != before {
		t.Fatalf("a parked call must stop strict compaction: err=%v", err)
	}

	// Between replies only.
	gate := newGatedTool("gate")
	busyModel := &seamModel{respond: func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		if call == 0 {
			return seamToolCalls(message.ToolCallBlock{ID: "g", Name: "gate"}), nil
		}
		return seamText("done"), nil
	}}
	busy := NewUnifiedAgent("a", "sys", busyModel, WithContextConfig(&ContextConfig{}), WithToolkit(tool.NewToolkit(gate)))
	ch, err := busy.ReplyStream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not start")
	}
	if _, err := busy.CompactStrict(context.Background(), CompactStrictOptions{}); err == nil {
		t.Fatal("CompactStrict must be rejected during a reply")
	}
	close(gate.gate)
	drain(t, ch)
}
