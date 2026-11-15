// The fixture uses only loopback HTTP and synthetic text, with no credentials
// or external service. Token/cost values and framing are synthetic evidence.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/managedtask"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func main() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 1 || request.Messages[0].Content != "Rewrite: hello" {
			http.Error(w, "fixture request mismatch", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"fixture-v1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hello."}}],"usage":{"prompt_tokens":12,"completion_tokens":2}}`)
	}))
	defer server.Close()
	client, err := model.NewOpenAITextClient(model.OpenAIConfig{APIKey: "local-fixture", Model: "fixture-v1", BaseURL: server.URL})
	if err != nil {
		panic(err)
	}
	defer client.CloseIdleConnections()
	executor, err := managedtask.New(managedtask.Config{
		Client:     client,
		Deployment: managedtask.Deployment{ID: "offline", Endpoint: server.URL + "/v1/chat/completions", Model: "fixture-v1", ContextWindow: 4096, TextOnly: true, NonStreaming: true, ToolsDisabled: true},
		Authorize: func(_ context.Context, task managedtask.Task, d managedtask.Deployment) error {
			if task.Mode != managedtask.Evaluation || d.Endpoint != server.URL+"/v1/chat/completions" {
				return fmt.Errorf("fixture authorization denied")
			}
			return nil
		},
		Project: func(_ context.Context, task managedtask.Task, _ managedtask.Deployment) (string, error) {
			// The host removes internal metadata before constructing the model request.
			text, _, _ := strings.Cut(task.Text, "\nINTERNAL:")
			return text, nil
		},
		InputTokenBound: func(_ context.Context, _ managedtask.Deployment, wire []byte) (int, error) {
			// Fixture-only contract: the local synthetic server accepts this byte bound
			// with 32 framing units. This is NOT a bound for any real model/tokenizer.
			return len(wire) + 32, nil
		},
	})
	if err != nil {
		panic(err)
	}
	result := executor.Execute(context.Background(), managedtask.Task{ID: "example-1", Category: "rewrite", Text: "Rewrite: hello\nINTERNAL: do not disclose", Mode: managedtask.Evaluation})
	fmt.Printf("status=%s text=%q attempts=%d unknown_cost_attempts=%d\n", result.Status, result.Text, len(result.Attempts), result.Cost.UnknownAttempts)
	if result.Err != nil {
		panic(result.Err)
	}
	// No qualification is supplied: production use is rejected before any request.
	rejected := executor.Execute(context.Background(), managedtask.Task{ID: "example-2", Category: "rewrite", Text: "Rewrite: hello"})
	fmt.Printf("production_status=%s attempts=%d\n", rejected.Status, len(rejected.Attempts))
}
