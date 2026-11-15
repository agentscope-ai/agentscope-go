package model_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// TestConnectivityAwareModel_Integration tests with a real cloud API.
// Requires DASHSCOPE_API_KEY environment variable.
// Run: DASHSCOPE_API_KEY=sk-xxx go test -v -run TestConnectivityAwareModel_Integration ./pkg/agentscope/model/
func TestConnectivityAwareModel_Integration(t *testing.T) {
	apiKey := os.Getenv("DASHSCOPE_API_KEY")
	if apiKey == "" {
		t.Skip("DASHSCOPE_API_KEY not set, skipping integration test")
	}

	// Cloud model: DashScope (real API)
	cloud, err := model.NewDashScopeChatModel(model.DashScopeConfig{
		APIKey: apiKey,
		Model:  "qwen-turbo",
	})
	if err != nil {
		t.Fatalf("failed to create cloud model: %v", err)
	}

	// Local model: a mock that always works (simulates Ollama)
	local := &localMockModel{}

	cam := model.NewConnectivityAwareModel(local, cloud,
		model.WithFailureThreshold(2),
		model.WithRecoveryTimeout(5*time.Second),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msgs := []*message.Msg{
		message.UserMsg("user", "Reply with exactly one word: hello"),
	}

	// Test 1: Cloud should work
	t.Run("CloudSuccess", func(t *testing.T) {
		resp, err := cam.Chat(ctx, msgs)
		if err != nil {
			t.Fatalf("cloud call failed: %v", err)
		}
		text := resp.GetTextContent()
		if text == "" {
			t.Error("expected non-empty response from cloud")
		}
		t.Logf("Cloud response: %q", text)

		if cam.ActiveModel() != "cloud" {
			t.Errorf("expected active model 'cloud', got %q", cam.ActiveModel())
		}
	})

	// Test 2: CountTokens should delegate to cloud
	t.Run("CountTokens", func(t *testing.T) {
		tokens := cam.CountTokens(msgs, nil)
		if tokens <= 0 {
			t.Errorf("expected positive token count, got %d", tokens)
		}
		t.Logf("Token count: %d", tokens)
	})
}

// TestConnectivityAwareModel_Integration_CloudToLocalSwitch verifies the
// fallback mechanism with a deliberately broken cloud endpoint.
func TestConnectivityAwareModel_Integration_CloudToLocalSwitch(t *testing.T) {
	apiKey := os.Getenv("DASHSCOPE_API_KEY")
	if apiKey == "" {
		t.Skip("DASHSCOPE_API_KEY not set, skipping integration test")
	}

	// Cloud model: broken endpoint (will always fail)
	brokenCloud, err := model.NewDashScopeChatModel(model.DashScopeConfig{
		APIKey:  "invalid-key-that-will-fail",
		BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
		Model:   "qwen-turbo",
	})
	if err != nil {
		t.Fatalf("failed to create broken cloud model: %v", err)
	}

	// Local model: mock
	local := &localMockModel{}

	cam := model.NewConnectivityAwareModel(local, brokenCloud,
		model.WithFailureThreshold(1),
		model.WithRecoveryTimeout(2*time.Second),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msgs := []*message.Msg{
		message.UserMsg("user", "hello"),
	}

	// Call should fail on cloud, fall back to local
	resp, err := cam.Chat(ctx, msgs)
	if err != nil {
		t.Fatalf("expected fallback to work, got error: %v", err)
	}

	text := resp.GetTextContent()
	if text != "local fallback response" {
		t.Errorf("expected local fallback response, got %q", text)
	}

	// After threshold, circuit should be open
	if cam.ActiveModel() != "local" {
		t.Errorf("expected active model 'local' after failures, got %q", cam.ActiveModel())
	}

	t.Log("Successfully fell back to local model after cloud failure")
}

// TestConnectivityAwareModel_Integration_Stream tests streaming with real API.
func TestConnectivityAwareModel_Integration_Stream(t *testing.T) {
	apiKey := os.Getenv("DASHSCOPE_API_KEY")
	if apiKey == "" {
		t.Skip("DASHSCOPE_API_KEY not set, skipping integration test")
	}

	cloud, err := model.NewDashScopeChatModel(model.DashScopeConfig{
		APIKey: apiKey,
		Model:  "qwen-turbo",
	})
	if err != nil {
		t.Fatalf("failed to create cloud model: %v", err)
	}

	local := &localMockModel{}
	cam := model.NewConnectivityAwareModel(local, cloud,
		model.WithFailureThreshold(3),
		model.WithRecoveryTimeout(10*time.Second),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msgs := []*message.Msg{
		message.UserMsg("user", "Count from 1 to 3, one number per line."),
	}

	ch, err := cam.ChatStream(ctx, msgs)
	if err != nil {
		t.Fatalf("stream setup failed: %v", err)
	}

	var chunks int
	var fullText string
	for resp := range ch {
		if resp.Error != nil {
			t.Fatalf("stream error: %v", resp.Error)
		}
		chunks++
		fullText += resp.GetTextContent()
	}

	if chunks == 0 {
		t.Error("expected at least one chunk")
	}
	if fullText == "" {
		t.Error("expected non-empty streamed text")
	}
	t.Logf("Received %d chunks, text: %q", chunks, fullText)
}

// localMockModel simulates a local Ollama model that always works.
type localMockModel struct{}

func (m *localMockModel) Chat(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	return &model.ChatResponse{
		Content:   []message.ContentBlock{message.TextBlock{Type: "text", Text: "local fallback response"}},
		IsLast:    true,
		ModelName: "local-mock",
	}, nil
}

func (m *localMockModel) ChatStream(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	ch := make(chan model.ChatResponse, 1)
	ch <- model.ChatResponse{
		Content:   []message.ContentBlock{message.TextBlock{Type: "text", Text: "local stream"}},
		IsLast:    true,
		ModelName: "local-mock",
	}
	close(ch)
	return ch, nil
}

func (m *localMockModel) CountTokens(msgs []*message.Msg, _ []model.ToolSchema) int {
	return len(msgs) * 5
}
