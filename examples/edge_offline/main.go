// Example edge_offline demonstrates the ConnectivityAwareModel for edge deployments.
//
// It creates a model that prefers a cloud API when available, but automatically
// falls back to a local Ollama instance when connectivity is lost. When the cloud
// comes back, the model switches back automatically via the circuit breaker.
// The local model uses Ollama's native /api/chat endpoint (OllamaNativeChatModel).
//
// Run:
//
//	# Start Ollama locally first:
//	ollama serve &
//	ollama pull qwen2.5:0.5b
//
//	# With cloud available:
//	export OPENAI_API_KEY=sk-...
//	go run ./examples/edge_offline/
//
//	# Without cloud (offline mode):
//	go run ./examples/edge_offline/
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func main() {
	// Local model: Ollama on localhost (always available on the edge device),
	// via the native /api/chat endpoint.
	local, err := model.NewOllamaNativeChatModel(model.OllamaNativeConfig{
		Model: "qwen2.5:0.5b",
	})
	if err != nil {
		fmt.Println("Failed to create local model:", err)
		os.Exit(1)
	}

	// Cloud model: OpenAI (or any cloud provider) — needs internet.
	var cloud model.ChatModel
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		cloud, err = model.NewOpenAIChatModel(model.OpenAIConfig{
			APIKey: key,
			Model:  "gpt-4o-mini",
		})
		if err != nil {
			fmt.Println("Failed to create cloud model:", err)
			os.Exit(1)
		}
	} else {
		// No cloud key — use a second Ollama model as "cloud" for demo purposes
		// (kept on the OpenAI-compatible adapter; both adapters are ChatModel).
		cloud, err = model.NewOllamaChatModel(model.OllamaConfig{
			BaseURL: "http://cloud-that-does-not-exist:11434",
			Model:   "qwen2.5:0.5b",
		})
		if err != nil {
			fmt.Println("Failed to create cloud model:", err)
			os.Exit(1)
		}
	}

	// Create connectivity-aware model.
	cam := model.NewConnectivityAwareModel(local, cloud,
		model.WithFailureThreshold(2),
		model.WithRecoveryTimeout(10*time.Second),
	)

	ctx := context.Background()
	msgs := []*message.Msg{
		message.UserMsg("user", "What is 2+2? Answer in one word."),
	}

	fmt.Printf("Active model: %s\n", cam.ActiveModel())
	fmt.Println("Sending request...")

	resp, err := cam.Chat(ctx, msgs)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Active model after call: %s\n", cam.ActiveModel())
	fmt.Printf("Response: %s\n", resp.GetTextContent())

	// Demonstrate the switch: make a few more calls to show stability.
	for i := 0; i < 2; i++ {
		time.Sleep(500 * time.Millisecond)
		resp, err = cam.Chat(ctx, msgs)
		if err != nil {
			fmt.Printf("Call %d error: %v\n", i+1, err)
			continue
		}
		fmt.Printf("Call %d [%s]: %s\n", i+1, cam.ActiveModel(), resp.GetTextContent())
	}
}
