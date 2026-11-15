package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// A syntactically usable summary must still be rejected when the real adapter
// reports that generation was truncated or filtered.
func TestForkTextBufferedOpenAIStopReason(t *testing.T) {
	for _, reason := range []string{"stop", "length", "content_filter"} {
		t.Run(reason, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"id":"r1","choices":[{"message":{"role":"assistant","content":"candidate summary"},"finish_reason":%q}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`, reason)
			}))
			defer srv.Close()
			m, err := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "test", Model: "test", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := m.Chat(context.Background(), []*message.Msg{message.UserMsg("u", "summarize")})
			if err != nil {
				t.Fatal(err)
			}
			text, err := forkText(resp)
			if reason == "stop" {
				if err != nil || text != "candidate summary" {
					t.Fatalf("text=%q err=%v", text, err)
				}
			} else if err == nil || text != "" {
				t.Fatalf("incomplete summary accepted: text=%q err=%v", text, err)
			}
			if resp.GetTextContent() != "candidate summary" || resp.Usage == nil || resp.Usage.InputTokens != 2 || resp.Usage.OutputTokens != 3 {
				t.Fatalf("partial response/usage lost: %+v", resp)
			}
		})
	}
}
