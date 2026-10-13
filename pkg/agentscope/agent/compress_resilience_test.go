package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/resilience"
)

func TestCompressContext_ResilienceWrappedWindow(t *testing.T) {
	breaker := func() resilience.ResilienceOption {
		return resilience.WithCircuitBreaker(resilience.NewCircuitBreaker(3, time.Minute))
	}
	tests := []struct {
		name          string
		primaryWindow int
		wrapPrimary   bool
		fallback      model.ChatModel
	}{
		{"wrapped agent model", 8192, true, nil},
		{"wrapped fallback model", 32000, false, resilience.Wrap(&compressionMockModel{contextSize: 8192}, breaker())},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The primary model counts the tokens: 20000 is above 0.8*8192
			// and below 0.8*32000, so only the wrapped 8192 window triggers
			// compression.
			primary := &compressionMockModel{tokenCount: 20000, contextSize: tt.primaryWindow}
			var agentModel model.ChatModel = primary
			if tt.wrapPrimary {
				agentModel = resilience.Wrap(primary, breaker())
			}
			a := NewUnifiedAgent("test", "prompt", agentModel,
				WithModelConfig(ModelConfig{FallbackModel: tt.fallback}),
				WithContextConfig(&ContextConfig{}),
			)
			for i := 0; i < 10; i++ {
				a.state.Context = append(a.state.Context,
					message.UserMsg("user", fmt.Sprintf("message %d", i)),
					message.AssistantMsg("bot", fmt.Sprintf("reply %d", i)),
				)
			}

			if err := a.compressContext(context.Background()); err != nil {
				t.Fatal(err)
			}

			if calls := atomic.LoadInt64(&primary.chatCallCount); calls == 0 {
				t.Fatal("context was not compressed (summary calls 0), want compression at the wrapped 8192 window")
			}
			if a.state.Summary == "" {
				t.Error("summary should be set after compression")
			}
		})
	}
}
