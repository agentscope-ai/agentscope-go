package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/runctx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type structuredUsageModel struct {
	call func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error)
}

func (m *structuredUsageModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	var o model.CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	return m.call(ctx, msgs, o)
}
func (*structuredUsageModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (*structuredUsageModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 0 }

type structuredUsageMiddleware struct {
	middleware.BaseMiddleware
	call func(context.Context, *middleware.ModelCallInput, middleware.ModelCallHandler) (*model.ChatResponse, error)
}

func (m *structuredUsageMiddleware) OnModelCall(ctx context.Context, in *middleware.ModelCallInput, next middleware.ModelCallHandler) (*model.ChatResponse, error) {
	return m.call(ctx, in, next)
}
func (*structuredUsageMiddleware) OnSystemPrompt(_ context.Context, _ string, p string) string {
	return p + " transformed"
}

func TestStructuredResponseUsageAndSnapshot(t *testing.T) {
	nested := map[string]any{"value": "original"}
	state := &AgentState{SessionID: "s", ReplyID: "r", Summary: "summary", Plan: []tool.PlanItem{{Step: "verify artifact", Status: "pending"}}, Context: []*message.Msg{message.AssistantMsg("a", []message.ContentBlock{message.ThinkingBlock{Type: "thinking", Extra: map[string]any{"nested": nested}}})}}
	calls := 0
	m := &structuredUsageModel{call: func(_ context.Context, msgs []*message.Msg, o model.CallOptions) (*model.ChatResponse, error) {
		calls++
		if len(msgs) != 4 {
			t.Fatalf("snapshot missing summary/plan: %d", len(msgs))
		}
		if got := msgs[0].GetTextContent(""); got == nil || *got != "system transformed" {
			t.Fatalf("prompt=%v", got)
		}
		tb := msgs[3].Content[0].(message.ThinkingBlock)
		if tb.Extra["nested"].(map[string]any)["value"] != "original" {
			t.Fatal("snapshot mutated")
		}
		tb.Extra["nested"].(map[string]any)["value"] = "changed"
		if calls == 1 {
			return &model.ChatResponse{Usage: &model.ChatUsage{InputTokens: 2}}, errors.New("tool_choice unsupported")
		}
		return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", Name: "generate_structured_output", Input: `{"report":"done"}`}}, Usage: &model.ChatUsage{InputTokens: 3, OutputTokens: 4}}, nil
	}}
	mw := &structuredUsageMiddleware{call: func(ctx context.Context, in *middleware.ModelCallInput, next middleware.ModelCallHandler) (*model.ChatResponse, error) {
		return next(ctx, in)
	}}
	a := NewUnifiedAgent("a", "system", m, WithState(state), WithMiddlewares(mw))
	r, err := a.GenerateStructuredResponseWithUsage(context.Background(), json.RawMessage(`{"type":"object"}`))
	if err != nil || !r.UsageComplete || r.Usage.InputTokens != 5 || r.Usage.OutputTokens != 4 {
		t.Fatalf("%+v %v", r, err)
	}
	if nested["value"] != "original" || len(state.Context) != 1 {
		t.Fatal("agent state mutated")
	}
}

func TestStructuredMiddlewareRejectionNeverFallsBack(t *testing.T) {
	n := 0
	m := &structuredUsageModel{call: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		n++
		t.Fatal("provider called")
		return nil, nil
	}}
	cause := errors.New("thinking denied by guardrail")
	mw := &structuredUsageMiddleware{call: func(context.Context, *middleware.ModelCallInput, middleware.ModelCallHandler) (*model.ChatResponse, error) {
		n++
		return nil, cause
	}}
	a := NewUnifiedAgent("a", "system", m, WithMiddlewares(mw))
	_, err := a.GenerateStructuredResponseWithUsage(context.Background(), json.RawMessage(`{"type":"object"}`))
	if !errors.Is(err, cause) || n != 1 {
		t.Fatalf("rejection retried: n=%d err=%v", n, err)
	}
}

func TestStructuredSchemaIsolationAndAmbiguousCalls(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	calls := 0
	m := &structuredUsageModel{call: func(_ context.Context, _ []*message.Msg, o model.CallOptions) (*model.ChatResponse, error) {
		calls++
		if string(o.Tools[0].Function.Parameters) != string(schema) {
			t.Fatal("provider mutated schema for next attempt")
		}
		o.Tools[0].Function.Parameters[0] = '!'
		if calls == 1 {
			return nil, errors.New("tool_choice unsupported")
		}
		return &model.ChatResponse{Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", Name: "generate_structured_output", Input: `{"result":"pass"}`},
			message.ToolCallBlock{Type: "tool_call", Name: "generate_structured_output", Input: `{"result":"fail"}`},
		}}, nil
	}}
	a := NewUnifiedAgent("a", "system", m)
	r, err := a.GenerateStructuredResponseWithUsage(context.Background(), schema)
	if err == nil || len(r.Output) != 0 {
		t.Fatalf("ambiguous verdict accepted: %+v %v", r, err)
	}
	if !json.Valid(schema) {
		t.Fatal("caller schema mutated")
	}
}

type structuredThinkingFixture struct{ *structuredUsageModel }

func (*structuredThinkingFixture) DisableThinkingOptions() []model.CallOption {
	return []model.CallOption{model.WithThinkingDisabled()}
}

func TestStructuredUsageBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		usage    *model.ChatUsage
		complete bool
	}{{"unknown", nil, false}, {"known_zero", &model.ChatUsage{}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			m := &structuredUsageModel{call: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
				return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: `{"report":"ok"}`}}, Usage: tc.usage}, nil
			}}
			a := NewUnifiedAgent("a", "system", m)
			r, err := a.GenerateStructuredResponseWithUsage(context.Background(), json.RawMessage(`{"type":"object"}`))
			if err != nil || r.UsageComplete != tc.complete || (r.Usage == nil) != (tc.usage == nil) {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
	calls := 0
	base := &structuredUsageModel{call: func(_ context.Context, _ []*message.Msg, o model.CallOptions) (*model.ChatResponse, error) {
		calls++
		if o.ThinkingEnable == nil || *o.ThinkingEnable {
			return nil, errors.New("thinking unsupported")
		}
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: `{"report":"ok"}`}}, Usage: &model.ChatUsage{InputTokens: 1}}, nil
	}}
	a := NewUnifiedAgent("a", "system", &structuredThinkingFixture{base})
	r, err := a.GenerateStructuredResponseWithUsage(context.Background(), json.RawMessage(`{"type":"object"}`))
	if err != nil || calls != 3 || r.UsageComplete {
		t.Fatalf("thinking capability lost: %+v %v calls=%d", r, err, calls)
	}
}

func TestStructuredFilteredErrorRetainsUsage(t *testing.T) {
	cause := errors.New("filter refused")
	m := &structuredUsageModel{call: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		return &model.ChatResponse{Usage: &model.ChatUsage{InputTokens: 7}}, nil
	}}
	mw := &structuredUsageMiddleware{call: func(ctx context.Context, in *middleware.ModelCallInput, next middleware.ModelCallHandler) (*model.ChatResponse, error) {
		_, _ = next(ctx, in)
		return nil, cause
	}}
	a := NewUnifiedAgent("a", "system", m, WithMiddlewares(mw))
	r, err := a.GenerateStructuredResponseWithUsage(context.Background(), json.RawMessage(`{"type":"object"}`))
	if !errors.Is(err, cause) || r.Usage.InputTokens != 7 || len(r.Output) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestStructuredSnapshotAndInputErrors(t *testing.T) {
	m := &structuredUsageModel{call: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		t.Fatal("unexpected model call")
		return nil, nil
	}}
	a := NewUnifiedAgent("a", "system", m)
	if _, err := a.GenerateStructuredResponseWithUsage(context.Background(), nil); err == nil {
		t.Fatal("invalid schema accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.GenerateStructuredResponseWithUsage(ctx, json.RawMessage("{}")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	a.state.Context = []*message.Msg{message.UserMsg("u", "x")}
	a.state.Context[0].Content = []message.ContentBlock{message.ThinkingBlock{Type: "thinking", Extra: map[string]any{"unsupported": make(chan int)}}}
	if _, err := a.GenerateStructuredResponseWithUsage(context.Background(), json.RawMessage("{}")); err == nil {
		t.Fatal("unsupported snapshot accepted")
	}
	a.activeReplies = 1
	if _, err := a.GenerateStructuredResponseWithUsage(context.Background(), json.RawMessage("{}")); err == nil {
		t.Fatal("busy agent accepted")
	}
}

func TestStructuredSharedReplyBudgetAndReplay(t *testing.T) {
	budget := middleware.NewReplyBudgetControl(5)
	calls := 0
	m := &structuredUsageModel{call: func(_ context.Context, msgs []*message.Msg, o model.CallOptions) (*model.ChatResponse, error) {
		calls++
		if calls == 1 {
			return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "work done"}}, Usage: &model.ChatUsage{InputTokens: 10}}, nil
		}
		if o.ToolChoice == nil || o.ToolChoice.Mode != "none" {
			t.Fatal("reply budget was reset or bypassed")
		}
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: `{"report":"done"}`}}, Usage: &model.ChatUsage{InputTokens: 4}}, nil
	}}
	a := NewUnifiedAgent("a", "system", m, WithMiddlewares(budget))
	ctx, err := middleware.WithReplyBudgetSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	workCtx, scope := runctx.New(ctx)
	if _, err = a.Reply(workCtx, "work"); err != nil {
		t.Fatal(err)
	}
	scope.Close()
	r, err := a.GenerateStructuredResponseWithUsage(ctx, json.RawMessage("{}"))
	if err != nil || r.Usage.InputTokens != 4 {
		t.Fatalf("%+v %v", r, err)
	}
	if got := middleware.SnapshotReplyBudgets(ctx).Counters[budget.Key()].UsedTokens; got != 14 {
		t.Fatalf("budget=%v", got)
	}
	cached := &structuredUsageMiddleware{call: func(context.Context, *middleware.ModelCallInput, middleware.ModelCallHandler) (*model.ChatResponse, error) {
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: `{"report":"cached"}`}}, Usage: &model.ChatUsage{}}, nil
	}}
	a = NewUnifiedAgent("a", "system", m, WithMiddlewares(cached))
	r, err = a.GenerateStructuredResponseWithUsage(context.Background(), json.RawMessage("{}"))
	if err != nil || calls != 2 || !r.UsageComplete {
		t.Fatalf("cache invoked provider: %+v %v calls=%d", r, err, calls)
	}
}

func TestStructuredCancellationKeepsKnownUsage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &structuredUsageModel{call: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		cancel()
		return &model.ChatResponse{Usage: &model.ChatUsage{InputTokens: 8}}, context.Canceled
	}}
	a := NewUnifiedAgent("a", "system", m)
	r, err := a.GenerateStructuredResponseWithUsage(ctx, json.RawMessage("{}"))
	if !errors.Is(err, context.Canceled) || r.Usage.InputTokens != 8 || len(r.Output) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestStructuredMixedCacheUsage(t *testing.T) {
	calls := 0
	m := &structuredUsageModel{call: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		calls++
		if calls == 1 {
			return &model.ChatResponse{Usage: &model.ChatUsage{InputTokens: 3, TotalInputTokens: 50, CacheInputTokens: 47, Time: 0.5}}, errors.New("tool_choice unsupported")
		}
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: `{"report":"ok"}`}}, Usage: &model.ChatUsage{InputTokens: 7, Time: 0.25}}, nil
	}}
	a := NewUnifiedAgent("a", "system", m)
	r, err := a.GenerateStructuredResponseWithUsage(context.Background(), json.RawMessage("{}"))
	if err != nil || r.Usage.TotalInputTokens != 57 || r.Usage.InputTokens != 10 || r.Usage.Time != 0.75 {
		t.Fatalf("cache-inclusive usage lost: %+v %v", r, err)
	}
}

func TestStructuredPreservesCompleteJSONText(t *testing.T) {
	for _, raw := range []string{
		`[{"result":"pass","message":"yes"},{"result":"fail","message":"no"}]`,
		`"a JSON string"`,
		`42`,
	} {
		t.Run(raw, func(t *testing.T) {
			m := &structuredUsageModel{call: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
				return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: raw}}}, nil
			}}
			a := NewUnifiedAgent("a", "system", m)
			r, err := a.GenerateStructuredResponseWithUsage(context.Background(), json.RawMessage(`{}`))
			if err != nil || string(r.Output) != raw {
				t.Fatalf("complete JSON changed: %+v %v; want %s", r, err, raw)
			}
		})
	}
}

func TestStructuredMiddleContextIsolation(t *testing.T) {
	for _, hostHasReply := range []bool{false, true} {
		for _, outcome := range []string{"success", "fallback", "provider_error", "middleware_error", "canceled", "no_agent_reply"} {
			name := "absent/" + outcome
			if hostHasReply {
				name = "existing/" + outcome
			}
			t.Run(name, func(t *testing.T) {
				host := middleware.MiddleContext{"host": map[string]any{"request": "original"}}
				want := middleware.MiddleContext{"host": map[string]any{"request": "original"}}
				if hostHasReply {
					host["agent"] = map[string]any{"reply_id": "host-reply", "other": "original"}
					want["agent"] = map[string]any{"reply_id": "host-reply", "other": "original"}
				}
				ctx, cancel := context.WithCancel(middleware.WithMiddleContext(context.Background(), host))
				defer cancel()
				failure := errors.New("fixture failure")
				calls, hooks := 0, 0
				replyID := "goal-reply"
				if outcome == "no_agent_reply" {
					replyID = ""
				}
				m := &structuredUsageModel{call: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
					calls++
					switch {
					case outcome == "fallback" && calls == 1:
						return nil, errors.New("tool_choice unsupported")
					case outcome == "provider_error":
						return nil, failure
					case outcome == "canceled":
						cancel()
						return nil, context.Canceled
					}
					return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: `{"report":"done"}`}}}, nil
				}}
				mw := &structuredUsageMiddleware{call: func(ctx context.Context, in *middleware.ModelCallInput, next middleware.ModelCallHandler) (*model.ChatResponse, error) {
					mc := middleware.GetMiddleContext(ctx)
					got, exists := mc.Get("agent", "reply_id")
					if exists != (replyID != "") || (exists && got != replyID) {
						t.Fatalf("helper reply ID: %v, %v; want %q", got, exists, replyID)
					}
					if _, inherited := mc["host"]; inherited {
						t.Error("helper reused caller middleware state")
					}
					if previous, ok := mc.Get("fixture", "calls"); hooks > 0 && (!ok || previous != hooks) {
						t.Error("compatibility attempts lost their middleware state")
					}
					hooks++
					mc.Set("fixture", "calls", hooks)
					if outcome == "middleware_error" {
						return nil, failure
					}
					return next(ctx, in)
				}}
				a := NewUnifiedAgent("a", "system", m, WithState(&AgentState{ReplyID: replyID}), WithMiddlewares(mw))
				r, err := a.GenerateStructuredResponseWithUsage(ctx, json.RawMessage(`{}`))
				if !reflect.DeepEqual(host, want) {
					t.Fatalf("caller MiddleContext changed: %#v; want %#v", host, want)
				}
				switch outcome {
				case "provider_error", "middleware_error":
					if !errors.Is(err, failure) {
						t.Fatal(err)
					}
				case "canceled":
					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				default:
					if err != nil || r == nil || string(r.Output) != `{"report":"done"}` {
						t.Fatalf("%+v %v", r, err)
					}
				}
				if outcome == "fallback" && calls != 2 {
					t.Fatalf("compatibility calls = %d; want 2", calls)
				}
			})
		}
	}
}
