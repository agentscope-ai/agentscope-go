package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestCompressContext_FallbackModelWindow(t *testing.T) {
	tests := []struct {
		name          string
		primaryWindow int
		fallback      model.ChatModel
		contextSize   int // explicit ContextConfig.ContextSize
		wantCompress  bool
	}{
		{"smaller fallback window", 32000, &compressionMockModel{contextSize: 8192}, 0, true},
		{"smaller primary window", 8192, &compressionMockModel{contextSize: 200000}, 0, true},
		{"explicit window wins", 32000, &compressionMockModel{contextSize: 8192}, 64000, false},
		{"larger fallback window", 32000, &compressionMockModel{contextSize: 200000}, 0, false},
		{"fallback reports no window", 32000, &mockChatModel{}, 0, false},
		{"no fallback", 32000, nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The primary model counts the tokens: 20000 is above 0.8*8192
			// and below 0.8*32000, so only an 8192 window on either side
			// triggers compression.
			primary := &compressionMockModel{tokenCount: 20000, contextSize: tt.primaryWindow}
			a := NewUnifiedAgent("test", "prompt", primary,
				WithModelConfig(ModelConfig{FallbackModel: tt.fallback}),
				WithContextConfig(&ContextConfig{ContextSize: tt.contextSize}),
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

			summaryCalls := atomic.LoadInt64(&primary.chatCallCount)
			if compressed := summaryCalls > 0; compressed != tt.wantCompress {
				t.Fatalf("compressed = %v (summary calls %d), want %v", compressed, summaryCalls, tt.wantCompress)
			}
			if tt.wantCompress && a.state.Summary == "" {
				t.Error("summary should be set after compression")
			}
		})
	}
}
