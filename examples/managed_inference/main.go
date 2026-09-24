// Run with go run ./examples/managed_inference. The default uses a local HTTP
// protocol fixture. Set -base-url and -model plus OPENAI_API_KEY for a real
// OpenAI Chat endpoint; the base URL excludes /v1, matching this adapter.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	endpoint := flag.String("base-url", "", "OpenAI Chat base URL, without /v1; empty runs a local fixture")
	name := flag.String("model", "fixture", "model served by the endpoint")
	flag.Parse()
	key := os.Getenv("OPENAI_API_KEY")
	if *endpoint == "" {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Stream bool `json:"stream"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, "invalid JSON", 400)
				return
			}
			if request.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fixture reply\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
				return
			}
			fmt.Fprint(w, `{"choices":[{"message":{"content":"fixture reply"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
		}))
		defer server.Close()
		*endpoint, key = server.URL, "fixture"
		fmt.Println("Local protocol fixture; no provider or throughput validation.")
	}
	ledger := inference.NewLedger(1000)
	controller := inference.NewController(ledger)
	defer controller.Close()
	if err := controller.RegisterPool(inference.PoolConfig{ID: "shared", MaxActive: 2, MaxQueued: 4, MaxQueuedWork: 4000}); err != nil {
		return err
	}
	target, err := controller.Register(&inference.DeploymentConfig{
		Descriptor: inference.Descriptor{ID: "primary", Provider: "openai-compatible", API: "openai-chat", Model: *name, Endpoint: *endpoint},
		PoolID:     "shared", MaxAttempts: 2,
	})
	if err != nil {
		return err
	}
	base, err := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: key, Model: *name, BaseURL: *endpoint})
	if err != nil {
		return err
	}
	managed, err := model.NewManagedChatModel(base, target)
	if err != nil {
		return err
	}
	defer managed.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// An application must supply this identity after authenticating its caller.
	ctx = inference.WithIdentity(ctx, inference.Identity{TenantID: "demo"})
	ctx = inference.WithEstimatedWork(ctx, 1000)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := agent.NewUnifiedAgent(fmt.Sprintf("worker-%d", i), "Answer briefly.", managed)
			_, err := a.Reply(ctx, "Say hello.")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}

	stream, err := managed.ChatStream(ctx, []*message.Msg{message.UserMsg("demo", "Say hello.")})
	if err != nil {
		return err
	}
	var streamErr error
	terminal := false
	for part := range stream {
		if part.Error != nil {
			streamErr = part.Error
		}
		if part.IsLast {
			terminal = true
		} // Final content is assembled, not another delta.
	}
	if streamErr != nil {
		return streamErr
	}
	if !terminal {
		return fmt.Errorf("stream closed without a final response (context: %v)", ctx.Err())
	}
	snapshot := ledger.Snapshot()
	fmt.Printf("operations=%d physical_attempts=%d unknown_usage=%d unknown_cost=%d incomplete=%v active=%d\n",
		len(snapshot.Operations), len(snapshot.Attempts), snapshot.UnknownUsage, snapshot.UnknownCost, snapshot.Incomplete, target.Stats().Active)
	return nil
}
