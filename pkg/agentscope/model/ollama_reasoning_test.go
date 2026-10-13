package model

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Fixtures follow the shapes Ollama's OpenAI-compatible endpoint produces
// (ollama openai/openai.go ToChatCompletion and ToChunks): thinking arrives
// as a string "reasoning" field on the message or delta.

func ollamaChatJSON(message string) string {
	return `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"qwen3:14b","system_fingerprint":"fp_ollama",` +
		`"choices":[{"index":0,"message":` + message + `,"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`
}

// ollamaStreamSSE renders one chunk per delta object, then the finish chunk,
// the usage chunk and [DONE].
func ollamaStreamSSE(deltas ...string) string {
	var b strings.Builder
	chunk := func(choices string) {
		fmt.Fprintf(&b, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"qwen3:14b","system_fingerprint":"fp_ollama","choices":%s}`+"\n\n", choices)
	}
	for _, d := range deltas {
		chunk(`[{"index":0,"delta":` + d + `,"finish_reason":null}]`)
	}
	chunk(`[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"stop"}]`)
	fmt.Fprint(&b, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"qwen3:14b","system_fingerprint":"fp_ollama","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`+"\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// blockSummary renders thinking and text blocks for comparison.
func blockSummary(blocks []message.ContentBlock) []string {
	var out []string
	for _, b := range blocks {
		switch v := b.(type) {
		case message.ThinkingBlock:
			out = append(out, "thinking:"+v.Thinking)
		case message.TextBlock:
			out = append(out, "text:"+v.Text)
		default:
			out = append(out, fmt.Sprintf("%T", b))
		}
	}
	return out
}

func TestOllamaReasoningChat(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    []string
	}{
		{"reasoning",
			`{"role":"assistant","content":"Hello","reasoning":"Let me think"}`,
			[]string{"thinking:Let me think", "text:Hello"}},
		{"reasoning_content wins when equal",
			`{"role":"assistant","content":"Hello","reasoning_content":"same","reasoning":"same"}`,
			[]string{"thinking:same", "text:Hello"}},
		{"reasoning_content wins when different",
			`{"role":"assistant","content":"Hello","reasoning_content":"rc","reasoning":"r"}`,
			[]string{"thinking:rc", "text:Hello"}},
		{"null reasoning",
			`{"role":"assistant","content":"Hello","reasoning":null}`,
			[]string{"text:Hello"}},
		{"object reasoning is ignored",
			`{"role":"assistant","content":"Hello","reasoning":{"effort":"low"}}`,
			[]string{"text:Hello"}},
		{"array reasoning is ignored",
			`{"role":"assistant","content":"Hello","reasoning":[{"type":"summary"}]}`,
			[]string{"text:Hello"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, responses := ollamaExchange(t, false, ollamaChatJSON(tc.message), nil)
			if got := blockSummary(responses[0].Content); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("content = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOllamaReasoningStream(t *testing.T) {
	cases := []struct {
		name       string
		deltas     []string
		wantDeltas []string // flattened non-final responses
		wantFinal  []string
	}{
		{"reasoning then content",
			[]string{
				`{"role":"assistant","content":"","reasoning":"Let me "}`,
				`{"role":"assistant","content":"","reasoning":"think"}`,
				`{"role":"assistant","content":"Hello"}`,
			},
			[]string{"thinking:Let me ", "thinking:think", "text:Hello"},
			[]string{"thinking:Let me think", "text:Hello"}},
		{"mixed chunk publishes thinking first",
			[]string{`{"role":"assistant","content":"Hi","reasoning":"hmm"}`},
			[]string{"thinking:hmm", "text:Hi"},
			[]string{"thinking:hmm", "text:Hi"}},
		{"reasoning_content wins",
			[]string{`{"role":"assistant","content":"x","reasoning_content":"rc","reasoning":"r"}`},
			[]string{"thinking:rc", "text:x"},
			[]string{"thinking:rc", "text:x"}},
		{"object reasoning is ignored",
			[]string{`{"role":"assistant","content":"Hello","reasoning":{"effort":"low"}}`},
			[]string{"text:Hello"},
			[]string{"text:Hello"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, responses := ollamaExchange(t, true, ollamaStreamSSE(tc.deltas...), nil)
			last := responses[len(responses)-1]
			if !last.IsLast {
				t.Fatalf("stream ended without a final response: %+v", last)
			}
			var deltas []string
			for _, r := range responses[:len(responses)-1] {
				deltas = append(deltas, blockSummary(r.Content)...)
			}
			if !reflect.DeepEqual(deltas, tc.wantDeltas) {
				t.Errorf("deltas = %q, want %q", deltas, tc.wantDeltas)
			}
			if got := blockSummary(last.Content); !reflect.DeepEqual(got, tc.wantFinal) {
				t.Errorf("final content = %q, want %q", got, tc.wantFinal)
			}
		})
	}
}

// Thinking from an earlier turn is not sent back in either field.
func TestOllamaHistoryOmitsReasoning(t *testing.T) {
	msgs := []*message.Msg{
		message.UserMsg("user", "hello"),
		{Name: "bot", Role: message.RoleAssistant, Content: []message.ContentBlock{
			message.ThinkingBlock{Type: "thinking", Thinking: "earlier thought"},
			message.TextBlock{Type: "text", Text: "earlier answer"},
		}},
		message.UserMsg("user", "again"),
	}
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", streaming), func(t *testing.T) {
			reply := ollamaChatJSON(`{"role":"assistant","content":"ok"}`)
			if streaming {
				reply = ollamaStreamSSE(`{"role":"assistant","content":"ok"}`)
			}
			body, _ := ollamaExchange(t, streaming, reply, msgs)
			sent, _ := body["messages"].([]any)
			if len(sent) == 0 {
				t.Fatalf("no messages in request: %v", body)
			}
			for i, m := range sent {
				fields, _ := m.(map[string]any)
				for _, key := range []string{"reasoning", "reasoning_content"} {
					if _, ok := fields[key]; ok {
						t.Errorf("message %d carries %q: %v", i, key, fields)
					}
				}
			}
		})
	}
}
