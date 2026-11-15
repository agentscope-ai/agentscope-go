package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// A negative ContextSize used to skip the "0 = auto-detect" check, so the
// threshold and reserve collapsed to zero and every round summarized and
// cleared the whole context.
func TestContextConfig_NegativeContextSizeAutoDetects(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 1000, contextSize: 32000}
	a := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{ContextSize: -1}),
	)
	a.state.Context = []*message.Msg{
		message.UserMsg("user", "hello"),
		message.AssistantMsg("bot", "hi"),
	}

	if err := a.compressContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt64(&mock.chatCallCount); n != 0 {
		t.Errorf("compression called the model %d times below the model's threshold", n)
	}
	if got := len(a.state.Context); got != 2 {
		t.Errorf("context length = %d, want 2 (unchanged)", got)
	}
}

func TestContextConfig_NegativeContextSizeUsesModelWindow(t *testing.T) {
	// 30000 estimated tokens exceed 0.8 x the model's 32000-token window.
	// With an empty context the agent reports the threshold it applied.
	mock := &compressionMockModel{tokenCount: 30000, contextSize: 32000}
	a := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{ContextSize: -1}),
	)

	err := a.compressContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "(25600 tokens)") {
		t.Fatalf("expected the model-derived 25600-token threshold, got %v", err)
	}
}
