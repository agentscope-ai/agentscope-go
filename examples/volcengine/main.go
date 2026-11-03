// Direct Ark Chat Completions usage. Requires ARK_API_KEY and ARK_MODEL.
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
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cm, err := model.NewVolcengineChatModel(&model.VolcengineConfig{
		SecretAPIKey: model.NewSecretStr(os.Getenv("ARK_API_KEY")),
		Model:        os.Getenv("ARK_MODEL"),
		BaseURL:      os.Getenv("ARK_BASE_URL"), // Optional complete API prefix.
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ch, err := cm.ChatStream(ctx, []*message.Msg{message.UserMsg("user", "Explain what an agent is in one sentence.")})
	if err != nil {
		return err
	}
	finished := false
	for response := range ch {
		if response.Error != nil {
			return response.Error
		}
		if response.IsLast {
			// The final response contains assembled content, not another delta.
			finished = true
			fmt.Println()
			if response.Usage != nil {
				fmt.Printf("Tokens: input=%d output=%d cached=%d\n", response.Usage.InputTokens, response.Usage.OutputTokens, response.Usage.CacheInputTokens)
			}
		} else {
			fmt.Print(response.GetTextContent())
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !finished {
		return fmt.Errorf("Ark stream closed without a final response")
	}
	return nil
}
