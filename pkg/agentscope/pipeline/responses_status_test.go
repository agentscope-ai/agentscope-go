package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// newResponsesCutModel is the real OpenAI Responses adapter against a server
// whose answers are cut at max_output_tokens. A structured-output request
// (the goal report) gets a completed function call, so only the work round
// is incomplete.
func newResponsesCutModel(t *testing.T) model.ChatModel {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
			Tools  []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Stream || r.URL.Path != "/v1/responses" {
			t.Errorf("request %s stream=%t, want buffered /v1/responses", r.URL.Path, req.Stream)
		}
		w.Header().Set("Content-Type", "application/json")
		for _, tl := range req.Tools {
			if tl.Name == "generate_structured_output" {
				fmt.Fprint(w, `{"id":"resp_report","object":"response","status":"completed","error":null,"incomplete_details":null,`+
					`"output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"generate_structured_output","arguments":"{\"report\":\"partial answer\"}","status":"completed"}],`+
					`"usage":{"input_tokens":5,"output_tokens":5,"total_tokens":10}}`)
				return
			}
		}
		fmt.Fprint(w, `{"id":"resp_cut","object":"response","status":"incomplete","error":null,`+
			`"incomplete_details":{"reason":"max_output_tokens"},"max_output_tokens":16,`+
			`"output":[{"type":"message","id":"msg_1","status":"incomplete","role":"assistant",`+
			`"content":[{"type":"output_text","text":"partial answer","annotations":[]}]}],`+
			`"usage":{"input_tokens":12,"output_tokens":16,"total_tokens":28}}`)
	}))
	t.Cleanup(srv.Close)
	m, err := model.NewOpenAIResponseModel(&model.OpenAIResponseConfig{APIKey: "fixture", Model: "gpt-4.1", BaseURL: srv.URL, MaxOutputTokens: 16})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// On the buffered agent path, a Responses answer cut at max_output_tokens
// emits model_partial_response, so a team member's partial answer becomes an
// error result and goal work fails instead of being accepted.
func TestResponsesIncompleteAnswerFailsTeamAndGoal(t *testing.T) {
	t.Run("team member", func(t *testing.T) {
		m := newResponsesCutModel(t)
		member := TeamMember{Name: "A", Description: "fixture specialist", Factory: func(context.Context) (*agent.UnifiedAgent, error) {
			return agent.NewUnifiedAgent("A", "fixture", m, teamOptions()...), nil
		}}
		p, err := NewTeamPipeline(teamLeader([]message.ToolCallBlock{teamCall("assignment", TeamAssignToolName, `{"member":"A","prompt":"task"}`)}, func(results []message.ToolResultBlock) error {
			if len(results) != 1 || results[0].State != message.ToolResultError || results[0].GetOutputText() != "pipeline: A final model round is incomplete" {
				return fmt.Errorf("partial member answer accepted: %+v", results)
			}
			return nil
		}), member)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := teamDrain(t, p, nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("goal executor", func(t *testing.T) {
		a := agent.NewUnifiedAgent("worker", "work", newResponsesCutModel(t), agent.WithModelStreaming(false))
		e, err := NewGoalExecutor(a, GoalAgentConfig{})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		report, err := e.Execute(ctx, "answer the question", "")
		if err == nil || !strings.Contains(err.Error(), "agent work did not complete") {
			t.Fatalf("partial work accepted: report=%+v err=%v", report, err)
		}
	})
}
