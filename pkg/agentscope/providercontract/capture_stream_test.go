package providercontract

import (
	"context"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// TestCaptureBodyStreamScenario checks the ScnCaptureBodyStream fixture of
// every registered harness directly: the request body must be recorded and
// the served SSE stream must still assemble into ExpectStreamText. The wall
// only reaches this scenario through MaxTokensWireFormat, which harnesses
// without a MaxTokensKey (Anthropic) skip, so their fixture branch would
// otherwise go unexercised (review of agentscope-go#9).
func TestCaptureBodyStreamScenario(t *testing.T) {
	harnesses := registeredHarnesses()
	for i := range harnesses {
		h := &harnesses[i]
		t.Run(h.Name, func(t *testing.T) {
			var captured []byte
			srv := newServer(h, ScnCaptureBodyStream, &captured)
			defer srv.Close()
			m, err := h.NewModel(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			ch, err := m.ChatStream(context.Background(), testMsgs())
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			var final strings.Builder
			lastCount := 0
			for resp := range ch {
				if resp.Error != nil {
					t.Fatalf("stream error: %v", resp.Error)
				}
				if !resp.IsLast {
					continue
				}
				lastCount++
				for _, b := range resp.Content {
					if tb, ok := b.(message.TextBlock); ok {
						final.WriteString(tb.Text)
					}
				}
			}
			if lastCount != 1 {
				t.Fatalf("IsLast responses = %d, want exactly 1", lastCount)
			}
			if len(captured) == 0 {
				t.Fatal("request body was not captured")
			}
			requireContains(t, captured, "contract test", "captured request carries the user message")
			if final.String() != h.ExpectStreamText {
				t.Errorf("assembled stream text = %q, want %q", final.String(), h.ExpectStreamText)
			}
		})
	}
}
