package loop

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
)

func BenchmarkLoopSingleIteration(b *testing.B) {
	mc := &mockModelCaller{
		responses: []*model.ChatResponse{
			{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "Hello"}}, IsLast: true},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mc.mu.Lock()
		mc.callCount = 0
		mc.mu.Unlock()
		l := New(WithModelCaller(mc), WithMaxIters(1))
		_, _ = l.RunSync(context.Background(), "hi")
	}
}

func BenchmarkStateTransition(b *testing.B) {
	for i := 0; i < b.N; i++ {
		IsValidTransition(protocol.StateReason, protocol.StateInspect)
		IsValidTransition(protocol.StateInspect, protocol.StateAct)
		IsValidTransition(protocol.StateAct, protocol.StateReason)
	}
}

func BenchmarkInspectResponse(b *testing.B) {
	content := []message.ContentBlock{
		message.TextBlock{Type: "text", Text: "thinking..."},
		message.ToolCallBlock{Type: "tool_use", ID: "tc1", Name: "bash", State: message.ToolCallPending},
		message.ToolCallBlock{Type: "tool_use", ID: "tc2", Name: "read", State: message.ToolCallPending},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		InspectResponse(content)
	}
}

func BenchmarkDefaultContextManagerAppend(b *testing.B) {
	cm := NewDefaultContextManager()
	msg := message.UserMsg("user", "hello world")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cm.Append(msg)
	}
}

func BenchmarkHookRunnerDispatch(b *testing.B) {
	var calls []string
	hooks := make([]Hook, 5)
	for idx := 0; idx < 5; idx++ {
		hooks[idx] = &testHook{name: "h", calls: &calls}
	}
	runner := NewHookRunner(hooks...)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		calls = calls[:0]
		runner.BeforeModelCall(protocol.StateReason, 0)
		runner.AfterModelCall(protocol.StateReason, 0, nil)
	}
}
