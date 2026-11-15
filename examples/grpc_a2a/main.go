package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/a2a/grpc"
)

// This example demonstrates TCP-based agent-to-agent communication.
// It shows how to:
// 1. Start a Server on a random port (localhost:0)
// 2. Register a message handler that echoes with a prefix
// 3. Create a Client that connects to the server
// 4. Send a request-response message from Client to Server
// 5. Print the response and clean up

func main() {
	fmt.Println("=== TCP Agent-to-Agent Communication Example ===")
	fmt.Println()

	// Step 1: Create and start the server.
	server, err := grpc.NewServer("127.0.0.1:0")
	if err != nil {
		fmt.Println("Error creating server:", err)
		return
	}
	fmt.Printf("Server listening on: %s\n", server.Addr())

	// Step 2: Register a handler that echoes messages with a prefix.
	server.OnMessage(func(msg *grpc.Message) *grpc.Message {
		// Extract the text payload.
		var text string
		_ = json.Unmarshal(msg.Payload, &text)

		// Build the response.
		responseText := fmt.Sprintf("Hello from Agent B: %s", text)
		responsePayload, _ := json.Marshal(responseText)

		return &grpc.Message{
			ID:        msg.ID, // echo the same ID so the client can correlate
			From:      "agent-b",
			To:        msg.From,
			Method:    "reply",
			Payload:   responsePayload,
			Timestamp: time.Now(),
		}
	})

	// Start listening in a background goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- server.Listen(ctx)
	}()

	// Give the server a moment to be ready.
	time.Sleep(50 * time.Millisecond)

	// Step 3: Create a client and connect to the server.
	client, err := grpc.NewClient(server.Addr())
	if err != nil {
		fmt.Println("Error creating client:", err)
		return
	}
	fmt.Printf("Client connected to: %s\n", server.Addr())
	fmt.Println()

	// Step 4: Send a message from Agent A to Agent B.
	payload, _ := json.Marshal("How are you today?")
	request := &grpc.Message{
		ID:        "msg-001",
		From:      "agent-a",
		To:        "agent-b",
		Method:    "chat",
		Payload:   payload,
		Timestamp: time.Now(),
	}

	fmt.Printf("Agent A sending: %s\n", string(request.Payload))

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sendCancel()

	response, err := client.Send(sendCtx, request)
	if err != nil {
		fmt.Println("Error sending message:", err)
		return
	}

	// Step 5: Print the response.
	var responseText string
	_ = json.Unmarshal(response.Payload, &responseText)

	fmt.Printf("Agent B replied: %s\n", responseText)
	fmt.Printf("  From: %s, To: %s, Method: %s\n", response.From, response.To, response.Method)
	fmt.Println()

	// Verify the echo pattern worked.
	if responseText == "Hello from Agent B: How are you today?" {
		fmt.Println("SUCCESS: Agent-to-agent communication working correctly!")
	}
	fmt.Println()

	// Step 6: Clean up.
	if err := client.Close(); err != nil {
		fmt.Println("Error closing client:", err)
	}
	if err := server.Close(); err != nil {
		fmt.Println("Error closing server:", err)
	}
	fmt.Println("Client and server closed.")
}
