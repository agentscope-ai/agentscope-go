package agent

import (
	"context"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/runctx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// The hosts in these tests send the answer to a request from a goroutine
// (go a.Submit...): the agent's answer channels hold one value, and when a
// stale answer is wrongly taken, the host's own answer stays unread and a
// blocking send would hang the test instead of failing it.

// replyMatchCall is a tool call whose ID the model repeats in every reply, as
// Gemini does when it numbers calls per response.
func replyMatchCall(name string) message.ToolCallBlock {
	return message.ToolCallBlock{Type: "tool_call", ID: "call_0", Name: name, Input: `{}`, State: message.ToolCallPending}
}

// replyMatchStep is a model response with two such calls, call_0 and call_1.
func replyMatchStep(name string) model.ChatResponse {
	second := replyMatchCall(name)
	second.ID = "call_1"
	return model.ChatResponse{Content: []message.ContentBlock{replyMatchCall(name), second}, IsLast: true}
}

// cancelAtRequest runs one reply and cancels it as soon as it asks the host
// for a confirmation or an external result, without answering. It returns the
// reply ID that request carried, once the reply's core has exited: the stream
// of a canceled reply can close first, and a core still running would share
// the agent's state with the next reply. The App joins its cores the same way
// (runctx scope).
func cancelAtRequest(t *testing.T, a *UnifiedAgent) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx, scope := runctx.New(ctx)
	stream, err := a.ReplyStream(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	replyID := ""
	for ev := range stream {
		switch e := ev.(type) {
		case event.RequireUserConfirmEvent:
			replyID = e.ReplyID
			cancel()
		case event.RequireExternalExecutionEvent:
			replyID = e.ReplyID
			cancel()
		}
	}
	cancel()
	scope.Close()
	if replyID == "" {
		t.Fatal("the first reply asked the host nothing")
	}
	return replyID
}

// TestLateAnswerToCanceledReplyIgnoresReusedCallID: a host answers a request
// after canceling the reply that made it, in one batch for both of that
// reply's calls. The next reply's model reuses both tool-call IDs, so on
// tool-call ID alone the late batch would approve, or answer, the next
// reply's calls: the first through the channel, the second through the stash
// of batched entries. The batch names the canceled reply and is dropped
// whole; the next reply waits for its own answers.
func TestLateAnswerToCanceledReplyIgnoresReusedCallID(t *testing.T) {
	t.Run("confirmation", func(t *testing.T) {
		echoExecutions = 0
		step := replyMatchStep("echo_a")
		final := model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true}
		a := NewUnifiedAgent("bot", "Help the user.", &mockChatModel{responses: []model.ChatResponse{step, step, final}},
			WithToolkit(tool.NewToolkit(echoToolFixtureA())),
			WithPermissionContext(permission.NewContext(permission.ModeDefault)),
			WithReactConfig(ReactConfig{MaxIters: 4}),
		)
		canceled := cancelAtRequest(t, a)
		late := event.UserConfirmResultEvent{ReplyID: canceled}
		for _, b := range step.Content {
			late.ConfirmResults = append(late.ConfirmResults, event.ConfirmResult{Confirmed: true, ToolCall: b.(message.ToolCallBlock)})
		}
		a.SubmitUserConfirm(&late)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := a.ReplyStream(ctx, "second")
		if err != nil {
			t.Fatal(err)
		}
		asked := map[string]string{}                 // call ID -> reply ID of its request
		ends := map[string]message.ToolResultState{} // call ID -> result
		for ev := range stream {
			switch e := ev.(type) {
			case event.RequireUserConfirmEvent:
				asked[e.ToolCalls[0].ID] = e.ReplyID
				// The host's own answer denies.
				go a.SubmitUserConfirm(&event.UserConfirmResultEvent{ReplyID: e.ReplyID,
					ConfirmResults: []event.ConfirmResult{{Confirmed: false, ToolCall: e.ToolCalls[0]}}})
			case event.ToolResultEndEvent:
				ends[e.ToolCallID] = e.State
			}
		}
		if ctx.Err() != nil {
			t.Fatal("the second reply timed out")
		}
		for _, id := range []string{"call_0", "call_1"} {
			if asked[id] == "" || asked[id] == canceled {
				t.Errorf("%s: asked under reply %q, canceled reply %q", id, asked[id], canceled)
			}
			if ends[id] != message.ToolResultDenied {
				t.Errorf("%s: result %q, want denied by the host's own answer", id, ends[id])
			}
		}
		if echoExecutions != 0 {
			t.Errorf("the canceled reply's approval ran the second reply's calls %d times", echoExecutions)
		}
	})

	t.Run("external result", func(t *testing.T) {
		step := replyMatchStep("question")
		final := model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true}
		a := NewUnifiedAgent("bot", "Help the user.", &mockChatModel{responses: []model.ChatResponse{step, step, final}},
			WithToolkit(tool.NewToolkit(&validatedExternalTool{BaseTool: tool.BaseTool{ToolName: "question"}})),
			WithPermissionContext(permission.NewContext(permission.ModeDefault)),
			WithReactConfig(ReactConfig{MaxIters: 4}),
		)
		canceled := cancelAtRequest(t, a)
		late := event.ExternalExecutionResultEvent{ReplyID: canceled}
		for _, b := range step.Content {
			call := b.(message.ToolCallBlock)
			late.ExecutionResults = append(late.ExecutionResults, message.ToolResultBlock{Type: "tool_result", ID: call.ID, Name: call.Name, State: message.ToolResultSuccess, Output: "late answer"})
		}
		a.SubmitExternalResult(&late)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := a.ReplyStream(ctx, "second")
		if err != nil {
			t.Fatal(err)
		}
		outputs := map[string]string{} // call ID -> result text
		for ev := range stream {
			switch e := ev.(type) {
			case event.RequireExternalExecutionEvent:
				call := e.ToolCalls[0]
				go a.SubmitExternalResult(&event.ExternalExecutionResultEvent{ReplyID: e.ReplyID,
					ExecutionResults: []message.ToolResultBlock{{Type: "tool_result", ID: call.ID, Name: call.Name, State: message.ToolResultSuccess, Output: "own answer"}}})
			case event.ToolResultTextDeltaEvent:
				outputs[e.ToolCallID] += e.Delta
			}
		}
		if ctx.Err() != nil {
			t.Fatal("the second reply timed out")
		}
		for _, id := range []string{"call_0", "call_1"} {
			if outputs[id] != "own answer" {
				t.Errorf("%s: the second reply's call got %q, want its own answer", id, outputs[id])
			}
		}
	})
}

// TestAnswerWithoutReplyIDStillMatches: hosts that leave ReplyID empty (the
// app confirm handler and the legacy service confirm endpoint send none) keep
// tool-call ID matching.
func TestAnswerWithoutReplyIDStillMatches(t *testing.T) {
	t.Run("confirmation", func(t *testing.T) {
		echoExecutions = 0
		mock := &mockChatModel{responses: []model.ChatResponse{
			{Content: []message.ContentBlock{replyMatchCall("echo_a")}, IsLast: true},
			{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true},
		}}
		a := NewUnifiedAgent("bot", "Help the user.", mock,
			WithToolkit(tool.NewToolkit(echoToolFixtureA())),
			WithPermissionContext(permission.NewContext(permission.ModeDefault)),
			WithReactConfig(ReactConfig{MaxIters: 3}),
		)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := a.ReplyStream(ctx, "go")
		if err != nil {
			t.Fatal(err)
		}
		for ev := range stream {
			if e, ok := ev.(event.RequireUserConfirmEvent); ok {
				go a.SubmitUserConfirm(&event.UserConfirmResultEvent{
					ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: e.ToolCalls[0]}}})
			}
		}
		if ctx.Err() != nil || echoExecutions != 1 {
			t.Fatalf("an approval without a reply ID: ran %d times, ctx %v", echoExecutions, ctx.Err())
		}
	})

	t.Run("external result", func(t *testing.T) {
		mock := &mockChatModel{responses: []model.ChatResponse{
			{Content: []message.ContentBlock{replyMatchCall("question")}, IsLast: true},
			{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true},
		}}
		a := NewUnifiedAgent("bot", "Help the user.", mock,
			WithToolkit(tool.NewToolkit(&validatedExternalTool{BaseTool: tool.BaseTool{ToolName: "question"}})),
			WithPermissionContext(permission.NewContext(permission.ModeDefault)),
			WithReactConfig(ReactConfig{MaxIters: 3}),
		)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := a.ReplyStream(ctx, "go")
		if err != nil {
			t.Fatal(err)
		}
		output := ""
		for ev := range stream {
			switch e := ev.(type) {
			case event.RequireExternalExecutionEvent:
				go a.SubmitExternalResult(&event.ExternalExecutionResultEvent{
					ExecutionResults: []message.ToolResultBlock{{Type: "tool_result", ID: e.ToolCalls[0].ID, Name: e.ToolCalls[0].Name, State: message.ToolResultSuccess, Output: "answer"}}})
			case event.ToolResultTextDeltaEvent:
				output += e.Delta
			}
		}
		if ctx.Err() != nil || output != "answer" {
			t.Fatalf("a result without a reply ID: got %q, ctx %v", output, ctx.Err())
		}
	})
}

// TestResumedCallAcceptsTheCheckpointReplyID: a reply started from a plain
// checkpoint (WithState, then ReplyStream) gets a new reply ID and asks the
// parked call again. The host may answer the request it saw before the
// restart, naming the checkpoint's reply ID, before the new request arrives.
// That answer reaches the re-asked call; one naming any other reply does not,
// and the call waits for an answer to the new request.
func TestResumedCallAcceptsTheCheckpointReplyID(t *testing.T) {
	cases := []struct {
		name    string
		replyID string
		early   bool // the answer sent before the reply wins
	}{
		{"checkpoint reply", "r-old", true},
		{"another reply", "r-other", false},
	}
	for _, tc := range cases {
		t.Run("confirmation/"+tc.name, func(t *testing.T) {
			echoExecutions = 0
			call := message.ToolCallBlock{Type: "tool_call", ID: "tc_ask", Name: "echo_a", Input: `{}`, State: message.ToolCallAsking}
			a := NewUnifiedAgent("cp-agent", "helpful", &mockChatModel{responses: []model.ChatResponse{
				{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "resumed fine"}}, IsLast: true},
			}},
				WithToolkit(tool.NewToolkit(echoToolFixtureA())),
				WithPermissionContext(permission.NewContext(permission.ModeDefault)),
				WithReactConfig(ReactConfig{MaxIters: 4}),
				WithState(&AgentState{
					SchemaVersion: StateSchemaVersion, SessionID: "sess", ReplyID: "r-old",
					Context: []*message.Msg{
						message.UserMsg("user", "do the thing"),
						message.AssistantMsg("cp-agent", []message.ContentBlock{call}),
					},
				}),
				WithStateSaver(newFakeStateSaver()),
			)
			a.SubmitUserConfirm(&event.UserConfirmResultEvent{ReplyID: tc.replyID,
				ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: call}}})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := a.ReplyStream(ctx, "continue")
			if err != nil {
				t.Fatal(err)
			}
			for ev := range stream {
				if e, ok := ev.(event.RequireUserConfirmEvent); ok {
					if e.ReplyID == "r-old" {
						t.Errorf("the resumed reply kept the checkpoint's reply ID")
					}
					// The answer to the new request denies.
					go a.SubmitUserConfirm(&event.UserConfirmResultEvent{ReplyID: e.ReplyID,
						ConfirmResults: []event.ConfirmResult{{Confirmed: false, ToolCall: e.ToolCalls[0]}}})
				}
			}
			if ctx.Err() != nil {
				t.Fatal("the resumed reply timed out")
			}
			want := 0
			if tc.early {
				want = 1
			}
			if echoExecutions != want {
				t.Fatalf("the re-asked call ran %d times, want %d", echoExecutions, want)
			}
		})

		t.Run("external result/"+tc.name, func(t *testing.T) {
			call := message.ToolCallBlock{Type: "tool_call", ID: "tc_ext", Name: "question", Input: `{}`, State: message.ToolCallSubmitted}
			a := NewUnifiedAgent("cp-agent", "helpful", &mockChatModel{responses: []model.ChatResponse{
				{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "resumed fine"}}, IsLast: true},
			}},
				WithToolkit(tool.NewToolkit(&validatedExternalTool{BaseTool: tool.BaseTool{ToolName: "question"}})),
				WithPermissionContext(permission.NewContext(permission.ModeDefault)),
				WithReactConfig(ReactConfig{MaxIters: 4}),
				WithState(&AgentState{
					SchemaVersion: StateSchemaVersion, SessionID: "sess", ReplyID: "r-old",
					Context: []*message.Msg{
						message.UserMsg("user", "do the thing"),
						message.AssistantMsg("cp-agent", []message.ContentBlock{call}),
					},
				}),
				WithStateSaver(newFakeStateSaver()),
			)
			a.SubmitExternalResult(&event.ExternalExecutionResultEvent{ReplyID: tc.replyID,
				ExecutionResults: []message.ToolResultBlock{{Type: "tool_result", ID: call.ID, Name: call.Name, State: message.ToolResultSuccess, Output: "answer before restart"}}})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := a.ReplyStream(ctx, "continue")
			if err != nil {
				t.Fatal(err)
			}
			output := ""
			for ev := range stream {
				switch e := ev.(type) {
				case event.RequireExternalExecutionEvent:
					go a.SubmitExternalResult(&event.ExternalExecutionResultEvent{ReplyID: e.ReplyID,
						ExecutionResults: []message.ToolResultBlock{{Type: "tool_result", ID: e.ToolCalls[0].ID, Name: e.ToolCalls[0].Name, State: message.ToolResultSuccess, Output: "answer after restart"}}})
				case event.ToolResultTextDeltaEvent:
					output += e.Delta
				}
			}
			if ctx.Err() != nil {
				t.Fatal("the resumed reply timed out")
			}
			want := "answer after restart"
			if tc.early {
				want = "answer before restart"
			}
			if output != want {
				t.Fatalf("the re-asked call got %q, want %q", output, want)
			}
		})
	}
}

// TestCanceledWaiterTakesNoAnswer: a canceled reply's core can outlive its
// stream, and the next reply can start meanwhile. A waiter whose context has
// ended returns without reading, so it never consumes (and drops) an answer
// queued for the next reply, even when the answer is ready at the same time.
func TestCanceledWaiterTakesNoAnswer(t *testing.T) {
	a := NewUnifiedAgent("bot", "Help the user.", &mockChatModel{}, WithPermissionContext(permission.NewContext(permission.ModeDefault)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 50; i++ {
		a.SubmitUserConfirm(&event.UserConfirmResultEvent{ReplyID: "next",
			ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: message.ToolCallBlock{ID: "call_0"}}}})
		if ok, _ := a.waitForConfirmation(ctx, answerIDs{reply: "canceled"}, "call_0"); ok || len(a.confirmCh) != 1 {
			t.Fatalf("a canceled confirmation waiter read the queue: ok=%v queued=%d", ok, len(a.confirmCh))
		}
		<-a.confirmCh

		a.SubmitExternalResult(&event.ExternalExecutionResultEvent{ReplyID: "next",
			ExecutionResults: []message.ToolResultBlock{{ID: "call_0", Output: "answer"}}})
		if got := a.waitForExternalResult(ctx, answerIDs{reply: "canceled"}, "call_0"); got != nil || len(a.externalCh) != 1 {
			t.Fatalf("a canceled external waiter read the queue: got=%v queued=%d", got, len(a.externalCh))
		}
		<-a.externalCh
	}
}

// TestGrantPromptIgnoresAnotherReplysApproval: the workspace-grant prompt for
// a jailed file tool asks under the call's ID too, so an approval queued for
// another reply must not grant the directory.
func TestGrantPromptIgnoresAnotherReplysApproval(t *testing.T) {
	root, outside := writeOutsideFile(t, "outside content")
	a := NewUnifiedAgent("grant-agent", "helpful", &mockChatModel{responses: []model.ChatResponse{
		readCallResponse("call_0", outside),
		finalTextResponse("gave up"),
	}},
		WithToolkit(tool.NewToolkit(tool.ReadTool())),
		WithPermissionContext(permission.NewContext(permission.ModeBypass)),
		WithReactConfig(ReactConfig{MaxIters: 4}),
	)
	a.SubmitUserConfirm(&event.UserConfirmResultEvent{ReplyID: "canceled-reply",
		ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: message.ToolCallBlock{ID: "call_0", Name: "Read"}}}})
	ctx, cancel := context.WithTimeout(tool.WithWorkspaceRoot(context.Background(), root), 5*time.Second)
	defer cancel()
	stream, err := a.ReplyStream(ctx, "read it")
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	var end *event.ToolResultEndEvent
	for ev := range stream {
		switch e := ev.(type) {
		case event.RequireUserConfirmEvent:
			asked = true
			go a.SubmitUserConfirm(&event.UserConfirmResultEvent{ReplyID: e.ReplyID,
				ConfirmResults: []event.ConfirmResult{{Confirmed: false, ToolCall: e.ToolCalls[0]}}})
		case event.ToolResultEndEvent:
			end = &e
		}
	}
	if ctx.Err() != nil || !asked {
		t.Fatalf("asked=%v ctx=%v", asked, ctx.Err())
	}
	if dirs := a.engine.GrantedDirectories(); len(dirs) > 0 || end == nil || end.State != message.ToolResultError {
		t.Fatalf("another reply's approval granted %v; result %+v", dirs, end)
	}
}

// TestCheckpointBatchAnswersOnlyReaskedCalls: after a plain checkpoint
// resume, the host answers the request it saw before the restart with a batch
// naming the checkpoint's reply, and the batch also carries an entry for
// call_1. The re-asked call_0 takes the batch and parks the call_1 entry. A
// new call_1 that the resumed reply's model then emits accepts only the new
// reply ID, so the parked entry must not answer it; the host's answer to the
// new request does.
func TestCheckpointBatchAnswersOnlyReaskedCalls(t *testing.T) {
	restored := func(call message.ToolCallBlock) AgentOption {
		return WithState(&AgentState{
			SchemaVersion: StateSchemaVersion, SessionID: "sess", ReplyID: "r-old",
			Context: []*message.Msg{
				message.UserMsg("user", "do the thing"),
				message.AssistantMsg("cp-agent", []message.ContentBlock{call}),
			},
		})
	}
	responses := func(name string) []model.ChatResponse {
		next := replyMatchCall(name)
		next.ID = "call_1"
		return []model.ChatResponse{
			{Content: []message.ContentBlock{next}, IsLast: true},
			{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true},
		}
	}

	t.Run("confirmation", func(t *testing.T) {
		echoExecutions = 0
		parked := message.ToolCallBlock{Type: "tool_call", ID: "call_0", Name: "echo_a", Input: `{}`, State: message.ToolCallAsking}
		a := NewUnifiedAgent("cp-agent", "helpful", &mockChatModel{responses: responses("echo_a")},
			WithToolkit(tool.NewToolkit(echoToolFixtureA())),
			WithPermissionContext(permission.NewContext(permission.ModeDefault)),
			WithReactConfig(ReactConfig{MaxIters: 5}),
			restored(parked),
		)
		extra := parked
		extra.ID = "call_1"
		a.SubmitUserConfirm(&event.UserConfirmResultEvent{ReplyID: "r-old",
			ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: parked}, {Confirmed: true, ToolCall: extra}}})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := a.ReplyStream(ctx, "continue")
		if err != nil {
			t.Fatal(err)
		}
		ends := map[string]message.ToolResultState{}
		for ev := range stream {
			switch e := ev.(type) {
			case event.RequireUserConfirmEvent:
				if e.ToolCalls[0].ID == "call_1" {
					go a.SubmitUserConfirm(&event.UserConfirmResultEvent{ReplyID: e.ReplyID,
						ConfirmResults: []event.ConfirmResult{{Confirmed: false, ToolCall: e.ToolCalls[0]}}})
				}
			case event.ToolResultEndEvent:
				ends[e.ToolCallID] = e.State
			}
		}
		if ctx.Err() != nil {
			t.Fatal("the resumed reply timed out")
		}
		if echoExecutions != 1 || ends["call_0"] != message.ToolResultSuccess || ends["call_1"] != message.ToolResultDenied {
			t.Fatalf("ran %d times (want only call_0), results %v", echoExecutions, ends)
		}
	})

	t.Run("external result", func(t *testing.T) {
		parked := message.ToolCallBlock{Type: "tool_call", ID: "call_0", Name: "question", Input: `{}`, State: message.ToolCallSubmitted}
		a := NewUnifiedAgent("cp-agent", "helpful", &mockChatModel{responses: responses("question")},
			WithToolkit(tool.NewToolkit(&validatedExternalTool{BaseTool: tool.BaseTool{ToolName: "question"}})),
			WithPermissionContext(permission.NewContext(permission.ModeDefault)),
			WithReactConfig(ReactConfig{MaxIters: 5}),
			restored(parked),
		)
		before := func(id string) message.ToolResultBlock {
			return message.ToolResultBlock{Type: "tool_result", ID: id, Name: "question", State: message.ToolResultSuccess, Output: "answer before restart"}
		}
		a.SubmitExternalResult(&event.ExternalExecutionResultEvent{ReplyID: "r-old",
			ExecutionResults: []message.ToolResultBlock{before("call_0"), before("call_1")}})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := a.ReplyStream(ctx, "continue")
		if err != nil {
			t.Fatal(err)
		}
		outputs := map[string]string{}
		for ev := range stream {
			switch e := ev.(type) {
			case event.RequireExternalExecutionEvent:
				if call := e.ToolCalls[0]; call.ID == "call_1" {
					go a.SubmitExternalResult(&event.ExternalExecutionResultEvent{ReplyID: e.ReplyID,
						ExecutionResults: []message.ToolResultBlock{{Type: "tool_result", ID: call.ID, Name: call.Name, State: message.ToolResultSuccess, Output: "answer after restart"}}})
				}
			case event.ToolResultTextDeltaEvent:
				outputs[e.ToolCallID] += e.Delta
			}
		}
		if ctx.Err() != nil {
			t.Fatal("the resumed reply timed out")
		}
		if outputs["call_0"] != "answer before restart" || outputs["call_1"] != "answer after restart" {
			t.Fatalf("results %v", outputs)
		}
	})
}
