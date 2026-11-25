package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type geminiHistoryRequest struct {
	Contents []struct {
		Role  string `json:"role"`
		Parts []struct {
			Text         string `json:"text"`
			Signature    string `json:"thoughtSignature"`
			FunctionCall *struct {
				ID   string            `json:"id"`
				Name string            `json:"name"`
				Args map[string]string `json:"args"`
			} `json:"functionCall"`
			FunctionResponse *struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				Response struct {
					Result string `json:"result"`
				} `json:"response"`
			} `json:"functionResponse"`
		} `json:"parts"`
	} `json:"contents"`
}

func (request geminiHistoryRequest) describe() []string {
	segments := make([]string, 0, len(request.Contents))
	for _, content := range request.Contents {
		var parts []string
		for _, part := range content.Parts {
			switch {
			case part.FunctionCall != nil:
				call := part.FunctionCall
				parts = append(parts, fmt.Sprintf("call %s %s %s %s", call.ID, call.Name, call.Args["value"], part.Signature))
			case part.FunctionResponse != nil:
				result := part.FunctionResponse
				parts = append(parts, fmt.Sprintf("result %s %s %s", result.ID, result.Name, result.Response.Result))
			default:
				parts = append(parts, part.Text)
			}
		}
		segments = append(segments, content.Role+":"+strings.Join(parts, "|"))
	}
	return segments
}

func TestUnifiedAgentGeminiToolHistory(t *testing.T) {
	const signature = "c2lnLXNlbnRpbmVs"
	// The fixture returns an ordinary parallel step, a second tool step in
	// the same reply, and then a handoff that ends the next reply at its result.
	responseParts := []string{
		`{"text":"first step"},{"functionCall":{"id":"a","name":"lookup","args":{"value":"x"}},"thoughtSignature":"` + signature + `"},{"functionCall":{"id":"b","name":"lookup","args":{"value":"y"}}}`,
		`{"text":"second step"},{"functionCall":{"id":"c","name":"lookup","args":{"value":"z"}}}`,
		`{"text":"first answer"}`,
		`{"text":"handing off"},{"functionCall":{"id":"d","name":"transfer","args":{}}}`,
		`{"text":"follow-up answer"}`,
	}
	firstHistory := []string{
		"user:begin",
		"model:first step|call a lookup x " + signature + "|call b lookup y ",
		"user:result a lookup x|result b lookup y",
	}
	completedHistory := append(append([]string{}, firstHistory...),
		"model:second step|call c lookup z ", "user:result c lookup z", "model:first answer", "user:handoff",
	)
	expected := [][]string{
		{"user:begin"},
		firstHistory,
		completedHistory[:5],
		completedHistory,
		append(append([]string{}, completedHistory...), "model:handing off|call d transfer  ", "user:result d transfer transferred", "user:follow up"),
	}

	for _, streaming := range []bool{false, true} {
		mode := "buffered"
		if streaming {
			mode = "streaming"
		}
		t.Run(mode, func(t *testing.T) {
			var requestCount, executions atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				index := int(requestCount.Add(1)) - 1
				if index >= len(responseParts) {
					http.Error(w, "unexpected additional request", http.StatusBadRequest)
					return
				}
				var request geminiHistoryRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "decode", http.StatusBadRequest)
					return
				}
				if got, want := request.describe(), expected[index]; !reflect.DeepEqual(got, want) {
					t.Errorf("request %d contents:\n got %q\nwant %q", index+1, got, want)
				}
				body := fmt.Sprintf(`{"candidates":[{"content":{"role":"model","parts":[%s]},"finishReason":"STOP"}]}`, responseParts[index])
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\n", body)
					return
				}
				fmt.Fprint(w, body)
			}))
			defer server.Close()

			adapter, err := model.NewGeminiChatModel(model.GeminiConfig{
				APIKey: "test-only", Model: "test", BaseURL: server.URL, HTTPClient: server.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			lookup := tool.NewFunctionTool("lookup", "Look up a fixture value", nil, func(_ context.Context, args map[string]any) (any, error) {
				executions.Add(1)
				return args["value"], nil
			})
			var agent *UnifiedAgent
			transfer := tool.NewFunctionTool("transfer", "End this fixture reply", nil, func(context.Context, map[string]any) (any, error) {
				executions.Add(1)
				agent.MarkHandedOff()
				return "transferred", nil
			})
			agent = NewUnifiedAgent("fixture", "Use fixture tools.", adapter,
				WithToolkit(tool.NewToolkit(lookup, transfer)), WithModelStreaming(streaming))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			firstReply, err := agent.Reply(ctx, "begin")
			if err != nil {
				t.Fatal(err)
			}
			if len(firstReply.GetContentBlocks(message.ContentBlockToolResult)) != 3 {
				t.Fatalf("first reply lost tool results: %v", firstReply.Content)
			}
			handoffReply, err := agent.Reply(ctx, "handoff")
			if err != nil {
				t.Fatal(err)
			}
			parts := handoffReply.GetContentBlocks()
			if len(parts) == 0 || parts[len(parts)-1].GetType() != message.ContentBlockToolResult {
				t.Fatalf("handoff reply did not end with a tool result: %v", parts)
			}
			saved := len(agent.state.Context)
			before, err := json.Marshal(agent.state.Context)
			if err != nil {
				t.Fatal(err)
			}
			finalReply, err := agent.Reply(ctx, "follow up")
			if err != nil {
				t.Fatal(err)
			}
			if got := finalReply.GetTextContent(""); got == nil || *got != "follow-up answer" {
				t.Fatalf("unexpected final reply: %v", finalReply.Content)
			}
			after, err := json.Marshal(agent.state.Context[:saved])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("request replay mutated stored conversation history")
			}
			if requestCount.Load() != int32(len(expected)) || executions.Load() != 4 {
				t.Fatalf("requests=%d executions=%d", requestCount.Load(), executions.Load())
			}
		})
	}
}
