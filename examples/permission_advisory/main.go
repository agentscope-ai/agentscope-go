// A controlled, host-owned shadow reviewer. All chat/tool/user interactions
// are synthetic. -live changes only the classifier transport, not permissions.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"

	"github.com/agentscope-ai/agentscope-go/v2/internal/systemoneconfig"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/jev"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func main() {
	live := flag.Bool("live", false, "use an explicitly configured local classifier; chat, tool and user remain synthetic")
	enabled := flag.Bool("review", true, "install the advisory middleware (false demonstrates the inert path)")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, *live, *enabled, os.Getenv, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type scriptedModel struct {
	mu    sync.Mutex
	calls int
}

func (m *scriptedModel) Chat(context.Context, []*message.Msg, ...model.CallOption) (*model.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	var content []message.ContentBlock
	if m.calls == 1 {
		content = []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "fixture-call", Name: "read_fixture", Input: `{"target":"fixture.csv"}`, State: message.ToolCallPending}}
	} else {
		content = []message.ContentBlock{message.TextBlock{Type: "text", Text: "synthetic turn complete"}}
	}
	return &model.ChatResponse{Content: content, IsLast: true}, nil
}
func (*scriptedModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (*scriptedModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

type syntheticTool struct {
	tool.BaseTool
	executions atomic.Int32
}

func newSyntheticTool() *syntheticTool {
	return &syntheticTool{BaseTool: tool.BaseTool{ToolName: "read_fixture", ReadOnly: true, ConcurrencySafe: false}}
}
func (t *syntheticTool) Execute(context.Context, map[string]any) (*tool.ToolResponse, error) {
	t.executions.Add(1)
	return tool.NewTextResponse("synthetic CSV result; no file was accessed"), nil
}

type turnSummary struct {
	Stage              string `json:"stage"`
	Evidence           string `json:"evidence"`
	ScriptedUser       bool   `json:"scripted_user"`
	Confirmed          bool   `json:"confirmed"`
	ConfirmationEvents int    `json:"confirmation_events"`
	Executions         int32  `json:"synthetic_tool_executions"`
}

func run(ctx context.Context, live, enabled bool, getenv func(string) string, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if live && !enabled {
		return fmt.Errorf("-live requires -review=true")
	}
	var client classifier.DispatchClassifier
	evidence := "fixture"
	if enabled {
		var cfg *jev.Config
		if live {
			var err error
			cfg, err = systemoneconfig.Local(getenv)
			if err != nil {
				return err
			}
			if cfg == nil {
				return fmt.Errorf("-live requires local endpoint and served model")
			}
			evidence = "live-classifier"
		} else {
			server := classifierFixture()
			defer server.Close()
			cfg = &jev.Config{Endpoint: server.URL + "/v1/systemone", Model: "advisory-fixture", APIKey: "fixture-only"}
		}
		cl, err := jev.New(*cfg)
		if err != nil {
			return err
		}
		client = cl
	}
	// Two isolated Agents/scopes explicitly demonstrate both scripted outcomes.
	// No "always allow" rules or working-directory grants are submitted.
	for _, confirmed := range []bool{false, true} {
		target := newSyntheticTool()
		p := newPolicy(target)
		r, err := newReviewer(client, p, w, evidence)
		if err != nil {
			return err
		}
		a := agent.NewUnifiedAgent("demo", "Use the synthetic tool once.", &scriptedModel{}, agent.WithToolkit(tool.NewToolkit(target)), agent.WithPermissionContext(permission.NewContext(permission.ModeDefault)), agent.WithMiddlewares(r))
		p.engine = a.PermissionEngine()
		stream, err := a.ReplyStream(ctx, demoIntent)
		if err != nil {
			return err
		}
		asks := 0
		for e := range stream {
			if ev, ok := e.(event.RequireUserConfirmEvent); ok {
				asks++
				results := make([]event.ConfirmResult, 0, len(ev.ToolCalls))
				for _, call := range ev.ToolCalls {
					results = append(results, event.ConfirmResult{ToolCall: call, Confirmed: confirmed})
				}
				a.SubmitUserConfirm(&event.UserConfirmResultEvent{ConfirmResults: results})
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.auditError(); err != nil {
			return err
		}
		expected := int32(0)
		if confirmed {
			expected = 1
		}
		if asks != 1 || target.executions.Load() != expected {
			return fmt.Errorf("scripted confirmation invariant failed")
		}
		if err := json.NewEncoder(w).Encode(turnSummary{Stage: "summary", Evidence: evidence, ScriptedUser: true, Confirmed: confirmed, ConfirmationEvents: asks, Executions: target.executions.Load()}); err != nil {
			return err
		}
	}
	return nil
}

const fixtureResponse = `{"model":"advisory-fixture","answers":{"review":{"type":"choice","choice":"allow_once","confidence":0.9,"probabilities":{"allow_once":0.95,"needs_human":0.05}}},"usage":{"input_tokens":12,"output_tokens":0}}`

func classifierFixture() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/systemone" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, fixtureResponse)
	}))
}
