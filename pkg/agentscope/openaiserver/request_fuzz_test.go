package openaiserver_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/openaiserver"
)

// Exercise bounded, untrusted wire input at the real HTTP boundary. The fake
// provider never requests tools and needs no external services.
func FuzzTextRequests(f *testing.F) {
	for _, seed := range []string{basic, `null`, `{"messages":[]}`, `{"model":"support","messages":[{"role":"user","content":[{"type":"image_url"}]}]}`, strings.TrimSuffix(basic, "}") + `,"stream":true}`} {
		f.Add(seed)
	}
	h, err := openaiserver.New(openaiserver.Config{Models: map[string]model.ChatModel{"support": fixtureModel{}}, MaxBodyBytes: 4096, ReplyTimeout: time.Second})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, body string) {
		if len(body) > 8192 {
			t.Skip()
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if w.Code == 200 {
			if strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
				if !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
					t.Fatalf("unterminated successful stream: %s", w.Body)
				}
			} else {
				var response struct {
					Object  string
					Choices []json.RawMessage
				}
				if json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Object != "chat.completion" || len(response.Choices) != 1 {
					t.Fatalf("invalid successful completion: %s", w.Body)
				}
			}
		} else {
			var response struct {
				Error *struct{ Message, Type, Code string }
			}
			if w.Code < 400 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Error == nil || response.Error.Code == "" {
				t.Fatalf("invalid error response: %d %s", w.Code, w.Body)
			}
		}
	})
}
