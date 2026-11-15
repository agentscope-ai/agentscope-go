// Offline routing uses a simulated classifier and actual loopback HTTP attempts.
// Model identities, usage, context sizing and responses are synthetic fixtures.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/managedtask"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func main() {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(529)
		fmt.Fprint(w, `{"model":"fixture-v1","usage":{"prompt_tokens":4,"completion_tokens":0}}`)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Messages) != 1 || strings.Contains(request.Messages[0].Content, "INTERNAL") {
			http.Error(w, "bad projection", 400)
			return
		}
		fmt.Fprint(w, `{"model":"fixture-v1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Fixture completion."}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer backup.Close()
	var clients []*model.OpenAITextClient
	registration := func(id, base string) managedtask.Config {
		client, err := model.NewOpenAITextClient(model.OpenAIConfig{APIKey: "local-fixture", Model: "fixture-v1", BaseURL: base})
		if err != nil {
			panic(err)
		}
		clients = append(clients, client)
		endpoint, name := client.Target()
		return managedtask.Config{Client: client, Deployment: managedtask.Deployment{ID: id, Endpoint: endpoint, Model: name, ContextWindow: 4096, TextOnly: true, NonStreaming: true, ToolsDisabled: true},
			Authorize: func(_ context.Context, task managedtask.Task, _ managedtask.Deployment) error {
				if task.Mode != managedtask.Evaluation {
					return managedtask.ErrDenied
				}
				return nil
			},
			Project: func(_ context.Context, task managedtask.Task, _ managedtask.Deployment) (string, error) {
				text, _, _ := strings.Cut(task.Text, "\nINTERNAL:")
				return text, nil
			},
			InputTokenBound: func(_ context.Context, _ managedtask.Deployment, wire []byte) (int, error) {
				return len(wire) + 32, nil
			}, // fixture-only sizing contract
		}
	}
	deployments := []managedtask.Config{registration("primary", primary.URL), registration("backup", backup.URL)}
	defer func() {
		for _, client := range clients {
			client.CloseIdleConnections()
		}
	}()
	question := classifier.ChoiceQuestion{Instructions: "Classify only the permitted task text.", Choices: map[string]string{"extract": "Information extraction", "other": "Insufficient information"}}
	fake := classifier.NewFake(&classifier.FakeConfig{Response: classifier.Response{Model: "classifier-fixture-v1", Answers: map[string]classifier.Answer{"category": &classifier.ChoiceAnswer{Choice: "extract", Confidence: 0.9, Probabilities: map[string]float64{"extract": 0.9, "other": 0.1}}}}})
	endpoint, name := fake.Target()
	limits := managedtask.DefaultLimits()
	limits.MaxAttempts = 3
	router, err := managedtask.NewRouter(managedtask.RouterConfig{
		Deployments: deployments, Limits: &limits,
		Pools:      []managedtask.Pool{{ID: "extraction", Members: []managedtask.WeightedDeployment{{ID: "primary", Weight: 1}}, Fallback: []string{"backup"}}, {ID: "rewrite", Members: []managedtask.WeightedDeployment{{ID: "backup", Weight: 1}}}},
		Categories: map[string]string{"extract": "extraction", "rewrite": "rewrite"},
		Default:    &managedtask.DefaultRoute{Category: "rewrite", PoolID: "rewrite"},
		Rules: []managedtask.Rule{func(_ context.Context, task managedtask.Task) (string, error) {
			if strings.HasPrefix(task.Text, "Rewrite:") {
				return "rewrite", nil
			}
			return "", nil
		}},
		Classification: &managedtask.ClassificationConfig{Client: fake, Target: managedtask.ClassificationTarget{ID: "offline-classifier", Endpoint: endpoint, Model: name}, MaxAttempts: 1,
			Authorize: func(_ context.Context, task managedtask.Task, _ managedtask.ClassificationTarget) error {
				if task.Mode != managedtask.Evaluation {
					return managedtask.ErrDenied
				}
				return nil
			},
			Project: func(_ context.Context, task managedtask.Task, _ managedtask.ClassificationTarget) (classifier.Request, error) {
				text, _, _ := strings.Cut(task.Text, "\nINTERNAL:")
				state, _ := json.Marshal(text)
				return classifier.Request{State: state, Questions: map[string]classifier.Question{"category": &question}}, nil
			},
			Decisions: []managedtask.ChoiceDecision{{QuestionID: "category", Question: question, MinConfidence: 0.8, Categories: map[string]string{"extract": "extract", "other": ""}}}, // synthetic threshold, not calibrated for Jev
		},
	})
	if err != nil {
		panic(err)
	}
	for _, task := range []managedtask.Task{{ID: "rule", Text: "Rewrite: hello", Mode: managedtask.Evaluation}, {ID: "classified", Text: "Extract the name\nINTERNAL: private metadata", Mode: managedtask.Evaluation}, {ID: "default", Category: "unknown", Text: "Synthetic task", Mode: managedtask.Evaluation}, {ID: "production", Category: "extract", Text: "Synthetic task"}} {
		result := router.Execute(context.Background(), task)
		fmt.Printf("task=%s status=%s source=%s pool=%s deployment=%s attempts=%d unknown_cost_attempts=%d\n", task.ID, result.Status, result.Route.Source, result.Route.PoolID, result.Route.DeploymentID, len(result.Attempts), result.Cost.UnknownAttempts)
		for i := range result.Attempts {
			attempt := &result.Attempts[i]
			fmt.Printf("  attempt=%d kind=%s target=%s completed=%t\n", attempt.Number, attempt.Kind, attempt.DeploymentID, attempt.Completed)
		}
		if task.Mode == managedtask.Evaluation && result.Err != nil {
			panic(result.Err)
		}
	}
}
