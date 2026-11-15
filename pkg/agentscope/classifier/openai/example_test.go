package openai_test

import (
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/openai"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/credential"
)

// The model name follows the official Decisions guide (checked 2026-10-17):
// https://developers.openai.com/api/docs/guides/decisions
// This example constructs a client without making a live request.
func ExampleNew() {
	client, err := openai.New(openai.Config{
		Credential: &credential.OpenAICredential{Key: "example-key"},
		Model:      "gpt-6-luna", // Explicitly choose a model supported by Decisions.
	})
	if err != nil {
		panic(err)
	}
	endpoint, model := client.Target()
	fmt.Println(endpoint, model)
	// Output: https://api.openai.com/v1/decisions gpt-6-luna
}
