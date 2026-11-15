package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// This example mirrors Python scripts/model_examples/*_multimodal.py.
// It demonstrates sending images to vision-capable models using DataBlock
// with both URLSource and Base64Source.

const testImageURL = "https://help-static-aliyun-doc.aliyuncs.com/file-manage-files/zh-CN/20241022/emyrja/dog_and_girl.jpeg"

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	exampleImageURL(cm)
	exampleImageBase64(cm)
}

// ---------------------------------------------------------------------------
// Example 1: Image input via URL
// ---------------------------------------------------------------------------

func exampleImageURL(cm model.ChatModel) {
	fmt.Println("=== Multimodal Call (Image URL) ===")

	imageBlock := message.DataBlock{
		Type: "data",
		ID:   "img-1",
		Source: message.URLSource{
			Type:      "url",
			URL:       testImageURL,
			MediaType: "image/jpeg",
		},
	}

	msgs := []*message.Msg{
		message.NewMsg("user", message.RoleUser, []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "What animal is in this image? Describe it briefly."},
			imageBlock,
		}),
	}

	ch, err := cm.ChatStream(context.Background(), msgs)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	for resp := range ch {
		if !resp.IsLast {
			fmt.Print(resp.GetTextContent())
		} else {
			fmt.Println()
		}
	}
	fmt.Println()
}

// ---------------------------------------------------------------------------
// Example 2: Image input via base64
// ---------------------------------------------------------------------------

func exampleImageBase64(cm model.ChatModel) {
	fmt.Println("=== Multimodal Call (Base64 Image) ===")

	imgData, err := downloadImage(testImageURL)
	if err != nil {
		fmt.Println("Failed to download test image:", err)
		return
	}
	fmt.Printf("Downloaded %d bytes, encoding to base64...\n", len(imgData))

	b64 := base64.StdEncoding.EncodeToString(imgData)
	imageBlock := message.DataBlock{
		Type: "data",
		ID:   "img-2",
		Source: message.Base64Source{
			Type:      "base64",
			Data:      b64,
			MediaType: "image/jpeg",
		},
	}

	msgs := []*message.Msg{
		message.NewMsg("user", message.RoleUser, []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "What is happening in this image? Describe it briefly."},
			imageBlock,
		}),
	}

	ch, err := cm.ChatStream(context.Background(), msgs)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	for resp := range ch {
		if !resp.IsLast {
			fmt.Print(resp.GetTextContent())
		} else {
			fmt.Println()
		}
	}
	fmt.Println()
}

func downloadImage(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func loadChatModelFromEnv() (model.ChatModel, error) {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return model.NewAnthropicChatModel(&model.AnthropicConfig{
			APIKey: key, Model: "claude-sonnet-4-20250514", MaxOutputTokens: 1024,
			ClientOptions: &model.ClientOptions{Timeout: 120 * time.Second},
		})
	}
	if key := os.Getenv("DASHSCOPE_API_KEY"); key != "" {
		return model.NewDashScopeChatModel(model.DashScopeConfig{
			APIKey: key, BaseURL: os.Getenv("DASHSCOPE_BASE_URL"), Model: "qwen3.5-plus",
			ClientOptions: &model.ClientOptions{Timeout: 120 * time.Second},
		})
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		return model.NewOpenAIChatModel(model.OpenAIConfig{
			APIKey: key, Model: "gpt-4o-mini",
			ClientOptions: &model.ClientOptions{Timeout: 120 * time.Second},
		})
	}
	return nil, fmt.Errorf("set ANTHROPIC_API_KEY, DASHSCOPE_API_KEY, or OPENAI_API_KEY")
}
