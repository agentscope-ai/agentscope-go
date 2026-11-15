package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/providercontract"
)

// Capture real adapter requests, using the repository's offline HTTP/SSE
// harnesses. A successful fixture response never establishes provider acceptance.
func TestContentBlockCompactionAdapterRequests(t *testing.T) {
	for _, harness := range []providercontract.Harness{providercontract.OpenAIHarness(), providercontract.AnthropicHarness()} {
		for _, streaming := range []bool{false, true} {
			for _, separateResults := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/separate-results=%t", harness.Name, streaming, separateResults), func(t *testing.T) {
					thinking := message.ThinkingBlock{Type: "thinking", Thinking: "inspect both", Extra: map[string]any{"signature": "fixture-signature"}}
					calls := []message.ContentBlock{thinking, compactionCall("a", message.ToolCallFinished), compactionCall("b", message.ToolCallFinished)}
					results := []message.ContentBlock{compactionResult("a", message.ToolResultSuccess), compactionResult("b", message.ToolResultSuccess)}
					msgs := []*message.Msg{compactionMsg(append(compactionExchange("old"), append(calls, results...)...)...)}
					if separateResults {
						msgs = []*message.Msg{compactionMsg(compactionExchange("old")...), compactionMsg(calls...), message.NewMsg("tools", message.Role("tool"), results)}
					}
					a := NewUnifiedAgent("bot", "system", &blockCountModel{}, WithContentBlockCompaction(), WithContextConfig(&ContextConfig{ContextSize: 10, TriggerRatio: .6, ReserveRatio: .6}), WithState(&AgentState{Context: msgs}))
					if err := a.compressContext(context.Background()); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(blockResultIDs(a.state.Context), []string{"a", "b"}) {
						t.Fatal("fixture did not retain the parallel group")
					}
					captured := make(chan []byte, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var raw []byte
						scenario := providercontract.ScnCaptureBody
						if streaming {
							scenario = providercontract.ScnCaptureBodyStream
						}
						harness.Serve(w, r, scenario, &raw)
						captured <- raw
					}))
					defer server.Close()
					adapter, err := harness.NewModel(server.URL)
					if err != nil {
						t.Fatal(err)
					}
					input := a.prepareModelInput(context.Background())
					if streaming {
						stream, callErr := adapter.ChatStream(context.Background(), input)
						if callErr != nil {
							t.Fatal(callErr)
						}
						for response := range stream {
							if response.Error != nil {
								t.Fatal(response.Error)
							}
						}
					} else if _, err := adapter.Chat(context.Background(), input); err != nil {
						t.Fatal(err)
					}
					var request struct {
						Messages []struct {
							Role      string `json:"role"`
							ToolCalls []struct {
								ID string `json:"id"`
							} `json:"tool_calls"`
							ToolCallID string          `json:"tool_call_id"`
							Content    json.RawMessage `json:"content"`
						} `json:"messages"`
					}
					if err := json.Unmarshal(<-captured, &request); err != nil {
						t.Fatal(err)
					}
					pending := map[string]bool{}
					var resultIDs []string
					signedThinking := false
					for _, msg := range request.Messages {
						if harness.Name == "openai" {
							for _, call := range msg.ToolCalls {
								if msg.Role != "assistant" || (call.ID != "a" && call.ID != "b") {
									t.Fatalf("invalid retained call role/ID: %s %s", msg.Role, call.ID)
								}
								pending[call.ID] = true
							}
							if msg.ToolCallID != "" {
								if msg.Role != "tool" || !pending[msg.ToolCallID] {
									t.Fatalf("tool result has no preceding assistant call: %s", msg.ToolCallID)
								}
								delete(pending, msg.ToolCallID)
								resultIDs = append(resultIDs, msg.ToolCallID)
							}
							continue
						}
						var blocks []struct {
							Type      string `json:"type"`
							ID        string `json:"id"`
							ToolUseID string `json:"tool_use_id"`
							Signature string `json:"signature"`
						}
						if err := json.Unmarshal(msg.Content, &blocks); err != nil {
							t.Fatal(err)
						}
						for _, block := range blocks {
							switch block.Type {
							case "thinking":
								signedThinking = block.Signature == "fixture-signature"
							case "tool_use":
								if block.ID != "a" && block.ID != "b" {
									t.Fatalf("compressed call leaked onto wire: %s", block.ID)
								}
								pending[block.ID] = true
							case "tool_result":
								if !pending[block.ToolUseID] {
									t.Fatalf("result lacks retained call: %s", block.ToolUseID)
								}
								// Merged assistant histories have an existing Anthropic
								// role limitation. Only separated user results claim this
								// role invariant; both shapes check IDs and signatures.
								if separateResults && msg.Role != "user" {
									t.Fatal("separate Anthropic result is not a user message")
								}
								delete(pending, block.ToolUseID)
								resultIDs = append(resultIDs, block.ToolUseID)
							}
						}
					}
					if len(pending) != 0 || !reflect.DeepEqual(resultIDs, []string{"a", "b"}) {
						t.Fatalf("wire pairs: pending=%v results=%v", pending, resultIDs)
					}
					if harness.Name == "anthropic" && !signedThinking {
						t.Fatal("signed reasoning preceding the retained group was lost")
					}
				})
			}
		}
	}
}

func TestContentBlockCompactionCrossMessageThinkingWire(t *testing.T) {
	harness := providercontract.AnthropicHarness()
	for _, streaming := range []bool{false, true} {
		for _, budget := range []int{3, 4} {
			t.Run(fmt.Sprintf("stream=%t/budget=%d", streaming, budget), func(t *testing.T) {
				thinking := message.ThinkingBlock{Type: "thinking", Thinking: "inspect recent", Extra: map[string]any{"signature": "fixture-signature"}}
				msgs := crossMessageCompactionHistory(thinking)
				a := NewUnifiedAgent("bot", "system", &blockCountModel{}, WithContentBlockCompaction(), WithState(&AgentState{Context: msgs}),
					WithContextConfig(&ContextConfig{ContextSize: 10, TriggerRatio: .1, ReserveRatio: float64(budget) / 10}))
				if err := a.compressContext(context.Background()); err != nil {
					t.Fatal(err)
				}
				captured := make(chan []byte, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var raw []byte
					scenario := providercontract.ScnCaptureBody
					if streaming {
						scenario = providercontract.ScnCaptureBodyStream
					}
					harness.Serve(w, r, scenario, &raw)
					captured <- raw
				}))
				defer server.Close()
				adapter, err := harness.NewModel(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				input := a.prepareModelInput(context.Background())
				if streaming {
					stream, callErr := adapter.ChatStream(context.Background(), input)
					if callErr != nil {
						t.Fatal(callErr)
					}
					for response := range stream {
						if response.Error != nil {
							t.Fatal(response.Error)
						}
					}
				} else if _, err := adapter.Chat(context.Background(), input); err != nil {
					t.Fatal(err)
				}
				var request struct {
					Messages []struct {
						Role    string `json:"role"`
						Content []struct {
							Type      string `json:"type"`
							Thinking  string `json:"thinking"`
							Signature string `json:"signature"`
							ID        string `json:"id"`
							ToolUseID string `json:"tool_use_id"`
						} `json:"content"`
					} `json:"messages"`
				}
				if err := json.Unmarshal(<-captured, &request); err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, msg := range request.Messages {
					for _, block := range msg.Content {
						switch block.Type {
						case "thinking":
							if msg.Role != "assistant" || block.Thinking != thinking.Thinking || block.Signature != "fixture-signature" {
								t.Fatal("cross-message signed thinking was modified")
							}
						case "tool_use":
							if msg.Role != "assistant" || block.ID != "recent" {
								t.Fatal("unexpected retained tool call")
							}
						case "tool_result":
							if msg.Role != "user" || block.ToolUseID != "recent" {
								t.Fatal("unexpected retained tool result or role")
							}
						default:
							continue
						}
						got = append(got, block.Type)
					}
				}
				var want []string
				if budget == 4 {
					want = []string{"thinking", "tool_use", "tool_result"}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("wire group=%v, want %v: keep complete signed turn or wholesale-fallback", got, want)
				}
			})
		}
	}
}
