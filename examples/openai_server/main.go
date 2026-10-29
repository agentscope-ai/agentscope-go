// Serve a simulated order assistant through OpenAI Chat Completions.
// No provider credentials or database are required. Run with -self-test for
// a finite offline demonstration, or without it to listen on localhost.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/openaiserver"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	selfTest := flag.Bool("self-test", false, "run one offline request and exit")
	flag.Parse()
	h, err := openaiserver.New(openaiserver.Config{
		Models:        map[string]model.ChatModel{"order-assistant": demoModel{}},
		MaxConcurrent: 4, ReplyTimeout: 30 * time.Second,
		AgentFactory: func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
			lookup := tool.NewFunctionTool("lookup_order", "Look up order 123", json.RawMessage(`{"type":"object","properties":{}}`), func(context.Context, map[string]any) (any, error) { return "Order 123 is out for delivery.", nil })
			return r.NewAgent("orders", openaiserver.AgentConfig{Toolkit: tool.NewToolkit(lookup)}), nil
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	if *selfTest {
		if err := runOffline(h); err != nil {
			log.Fatal(err)
		}
		return
	}
	// Production hosts mount h behind their own authentication middleware.
	srv := &http.Server{Addr: *addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 20}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			log.Print(err)
		}
	}()
	fmt.Printf("Simulated order assistant: http://%s/v1/chat/completions\n", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Print(err)
	}
}

func runOffline(h http.Handler) error {
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"order-assistant","messages":[{"role":"user","content":"Where is order 123?"}]}`))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %s", resp.Status)
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}

// demoModel simulates a model choosing a registered tool, then answering from
// its result. Replace this with a configured provider model for real inference.
type demoModel struct{}

func (demoModel) Chat(_ context.Context, msgs []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	for _, msg := range msgs {
		for _, block := range msg.Content {
			if result, ok := block.(message.ToolResultBlock); ok {
				return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: result.GetOutputText()}}}, nil
			}
		}
	}
	return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "order-lookup", Name: "lookup_order", Input: "{}", State: message.ToolCallPending}}}, nil
}
func (m demoModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	resp, err := m.Chat(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	ch := make(chan model.ChatResponse, 1)
	ch <- *resp
	close(ch)
	return ch, nil
}
func (demoModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }
