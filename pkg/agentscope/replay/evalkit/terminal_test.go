package evalkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

type terminalModel struct {
	model.ChatModel
	calls int
}

func (m *terminalModel) Chat(context.Context, []*message.Msg, ...model.CallOption) (*model.ChatResponse, error) {
	m.calls++
	return nil, errors.New("fixture model failure")
}
func (*terminalModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

func TestRunTaskFailureCannotPassBudget(t *testing.T) {
	m := &terminalModel{}
	r := (&Runner{}).RunTask(context.Background(), &TaskSpec{ID: "failure", Input: "test", Scorer: ScorerSpec{Ref: "budget"}}, m)
	if r.Pass || r.Error == "" || r.Score != 0 {
		t.Fatalf("failed execution scored successful: %+v", r)
	}
}
func TestCollectOutcomeRejectsIncompleteReply(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  *event.ReplyEndEvent
	}{
		{"missing", nil},
		{"error", ptrEnd(event.NewReplyEndEventWithError("s", "r", types.ErrorUpstream, "failed"))},
		{"interrupted", ptrEnd(event.NewReplyEndEventWithReason("s", "r", types.ReplyInterrupted))},
		{"iterations", ptrEnd(event.NewReplyEndEventWithReason("s", "r", types.ReplyExceedMaxIters))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan event.Event, 3)
			ch <- event.NewReplyStartEvent("s", "r", "bot", message.RoleAssistant)
			if tc.end != nil {
				ch <- *tc.end
			}
			close(ch)
			if out := collectOutcome(context.Background(), ch); out.Error == "" {
				t.Fatalf("incomplete stream accepted: %+v", out)
			}
		})
	}
}
func ptrEnd[T any](e T) *T { return &e }

func TestCollectOutcomeTerminalBoundaries(t *testing.T) {
	for _, mode := range []string{"partial", "pending", "mismatched", "unknown", "canceled", "success"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch := make(chan event.Event, 8)
			ch <- event.NewReplyStartEvent("s", "r", "bot", message.RoleAssistant)
			ch <- event.NewTextBlockDeltaEvent("r", "text", "partial answer")
			switch mode {
			case "partial":
				ch <- event.NewCustomEvent("r", "model_partial_response", map[string]any{"error": "truncated"})
			case "pending":
				ch <- event.NewToolCallStartEvent("r", "call", "external")
			case "canceled":
				cancel()
			}
			end := event.NewReplyEndEvent("s", "r")
			if mode == "mismatched" {
				end.ReplyID = "other"
			}
			if mode == "unknown" {
				end.FinishedReason = "future"
			}
			ch <- end
			close(ch)
			out := collectOutcome(ctx, ch)
			if (out.Error == "") != (mode == "success") {
				t.Fatalf("mode=%s outcome=%+v", mode, out)
			}
			if out.FinalText != "partial answer" {
				t.Fatal("diagnostic partial text lost")
			}
		})
	}
}

type laterTurnFailure struct {
	model.ChatModel
	calls          int
	partial        bool
	firstCallDelay time.Duration
}

func (m *laterTurnFailure) Chat(ctx context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	m.calls++
	if m.calls == 1 && m.firstCallDelay > 0 {
		timer := time.NewTimer(m.firstCallDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if m.partial {
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "partial"}}, Error: errors.New("truncated")}, nil
	}
	if m.calls == 1 {
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "first"}}, Usage: &model.ChatUsage{InputTokens: 20, OutputTokens: 10}}, nil
	}
	return nil, errors.New("second turn failed")
}
func (*laterTurnFailure) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }
func TestRunTaskRejectsPartialAndLaterTurnFailure(t *testing.T) {
	for _, partial := range []bool{false, true} {
		m := &laterTurnFailure{partial: partial}
		task := &TaskSpec{ID: "turns", Input: "first", Turns: []string{"second", "never"}, Scorer: ScorerSpec{Ref: "budget"}}
		r := (&Runner{}).RunTask(context.Background(), task, m)
		if r.Pass || r.Error == "" || r.Score != 0 {
			t.Fatalf("failed task passed: %+v", r)
		}
		want := 2
		if partial {
			want = 1
		}
		if m.calls != want {
			t.Fatalf("continued after failure: %d calls", m.calls)
		}
		if !partial && (r.InputTokens != 20 || r.OutputTokens != 10) {
			t.Fatal("lost earlier turn usage")
		}
	}
}
func TestRunTaskSetupFailureRetainsEarlierUsage(t *testing.T) {
	// Give the latency-preservation assertion measurable work even on clocks
	// where an instantaneous fixture can start and finish in the same tick.
	m := &laterTurnFailure{firstCallDelay: 30 * time.Millisecond}
	r := (&Runner{}).RunTask(context.Background(), &TaskSpec{ID: "empty-turn", Input: "first", Turns: []string{""}, Scorer: ScorerSpec{Ref: "budget"}}, m)
	if r.Error == "" || r.Pass || r.InputTokens != 20 || r.OutputTokens != 10 || r.Iters != 1 || r.Latency == 0 {
		t.Fatalf("prior work lost: %+v", r)
	}
}

func (m terminalModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (m laterTurnFailure) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
