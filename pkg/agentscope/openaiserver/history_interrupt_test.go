package openaiserver_test

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// A client's historical tool call and its result reach the model as sent:
// the result, which the server stores in a message of its own, answers the
// call, so the new user message does not close the call as interrupted.
func TestHistoricalToolResultAnswersItsCall(t *testing.T) {
	var results []message.ToolResultBlock
	m := fixtureModel{chat: func(_ context.Context, msgs []*message.Msg, _ model.CallOptions) (*model.ChatResponse, error) {
		for _, msg := range msgs {
			for _, b := range msg.Content {
				if r, ok := b.(message.ToolResultBlock); ok {
					results = append(results, r)
				}
			}
		}
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}}, IsLast: true}, nil
	}}
	w := request(handler(t, m, nil), `{"model":"support","messages":[{"role":"user","content":"where?"},{"role":"assistant","content":null,"tool_calls":[{"id":"past","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"past","content":"delivered"},{"role":"assistant","content":"done"},{"role":"user","content":"next"}]}`)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if len(results) != 1 || results[0].ID != "past" || results[0].State != message.ToolResultSuccess || results[0].GetOutputText() != "delivered" {
		t.Fatalf("the model got results %+v, want only the client's", results)
	}
}
