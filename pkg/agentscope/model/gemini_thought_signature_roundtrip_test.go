package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Gemini thinking models require the function-call part's thoughtSignature
// echoed back with the same history part on the next request. The adapter
// must stash it in ToolCallBlock.Extra and the request builder must restore
// it on the wire; unsigned calls keep the plain part. Opaque values are
// synthetic sentinels, not valid provider signatures (upstream #2463).
func TestGeminiThoughtSignatureRoundTrip(t *testing.T) {
	const sig = "c2lnLXNlbnRpbmVs" // base64 of "sig-sentinel"
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			requests := 0
			var secondBody geminiRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var req geminiRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					http.Error(w, "decode", 400)
					return
				}
				if requests == 1 {
					// Two parallel calls: only the first carries a signature.
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"thought\":true,\"text\":\"thinking\"}]}}]}\n\n")
						fmt.Fprintf(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"get_weather\",\"args\":{\"city\":\"Tokyo\"}},\"thoughtSignature\":%q}]}}]}\n\n", sig)
						fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"get_weather\",\"args\":{\"city\":\"Paris\"}}}]},\"finishReason\":\"STOP\"}]}\n\n")
					} else {
						fmt.Fprintf(w, `{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"thinking"},{"functionCall":{"name":"get_weather","args":{"city":"Tokyo"}},"thoughtSignature":%q},{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]},"finishReason":"STOP"}]}`, sig)
					}
					return
				}
				secondBody = req
				if stream {
					fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"done\"}]},\"finishReason\":\"STOP\"}]}\n\n")
				} else {
					fmt.Fprint(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}]}`)
				}
			}))
			defer srv.Close()

			m, err := NewGeminiChatModel(GeminiConfig{APIKey: "test", Model: "test", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			msgs := []*message.Msg{message.UserMsg("u", "Weather?")}
			var resp *ChatResponse
			if stream {
				ch, e := m.ChatStream(context.Background(), msgs)
				if e != nil {
					t.Fatal(e)
				}
				for r := range ch {
					if r.Error != nil {
						t.Fatal(r.Error)
					}
					if r.IsLast {
						v := r
						resp = &v
					}
				}
			} else {
				resp, err = m.Chat(context.Background(), msgs)
				if err != nil {
					t.Fatal(err)
				}
			}

			var calls []message.ToolCallBlock
			for _, b := range resp.Content {
				if tc, ok := b.(message.ToolCallBlock); ok {
					calls = append(calls, tc)
				}
			}
			if len(calls) != 2 {
				t.Fatalf("calls=%+v", resp.Content)
			}
			signed := calls[0]
			if got, _ := signed.Extra["thought_signature"].(string); got != sig {
				t.Fatalf("signed call extra=%v, want %q", signed.Extra, sig)
			}
			plain := calls[1]
			if _, has := plain.Extra["thought_signature"]; has {
				t.Fatalf("unsigned call extra=%v", plain.Extra)
			}
			if signed.ID == plain.ID {
				t.Fatalf("call ids collide: %q", signed.ID)
			}

			// Persist/reload the message before replay to exercise the existing
			// serialized format (checkpoint safety).
			raw, e := json.Marshal(message.AssistantMsg("a", resp.Content))
			if e != nil {
				t.Fatal(e)
			}
			var assistant message.Msg
			if e = json.Unmarshal(raw, &assistant); e != nil {
				t.Fatal(e)
			}
			msgs = append(msgs, &assistant, message.NewMsg("tool", message.Role("tool"), []message.ContentBlock{
				message.ToolResultBlock{Type: "tool_result", ID: signed.ID, Name: "get_weather", Output: "sunny"},
				message.ToolResultBlock{Type: "tool_result", ID: plain.ID, Name: "get_weather", Output: "rainy"},
			}))

			if stream {
				ch, e := m.ChatStream(context.Background(), msgs)
				if e != nil {
					t.Fatal(e)
				}
				final := false
				for r := range ch {
					if r.Error != nil {
						t.Fatal(r.Error)
					}
					final = final || r.IsLast
				}
				if !final {
					t.Fatal("missing final response")
				}
			} else if _, err = m.Chat(context.Background(), msgs); err != nil {
				t.Fatal(err)
			}
			if requests != 2 {
				t.Fatalf("requests=%d", requests)
			}

			if len(secondBody.Contents) != 3 {
				t.Fatalf("contents=%+v", secondBody.Contents)
			}
			parts := secondBody.Contents[1].Parts
			if len(parts) != 2 || parts[0].FunctionCall == nil || parts[1].FunctionCall == nil {
				t.Fatalf("parts=%+v", parts)
			}
			if parts[0].ThoughtSignature != sig {
				t.Fatalf("part0 signature=%q, want %q", parts[0].ThoughtSignature, sig)
			}
			if parts[1].ThoughtSignature != "" {
				t.Fatalf("part1 signature=%q, want empty", parts[1].ThoughtSignature)
			}
			if parts[0].FunctionCall.Name != "get_weather" || parts[0].FunctionCall.Args["city"] != "Tokyo" {
				t.Fatalf("part0 call=%+v", parts[0].FunctionCall)
			}
			if parts[1].FunctionCall.Name != "get_weather" || parts[1].FunctionCall.Args["city"] != "Paris" {
				t.Fatalf("part1 call=%+v", parts[1].FunctionCall)
			}
		})
	}
}
