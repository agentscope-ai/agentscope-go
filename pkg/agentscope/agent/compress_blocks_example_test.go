package agent_test

import (
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func ExampleWithContentBlockCompaction() {
	cm, err := model.NewOpenAIChatModel(model.OpenAIConfig{
		APIKey: "configured-by-host", Model: "configured-model",
	})
	if err != nil {
		return
	}
	_ = agent.NewUnifiedAgent("analyst", "Analyze diagnostic logs.", cm,
		agent.WithContextConfig(&agent.ContextConfig{
			ContextSize: 4096, TriggerRatio: 0.8,
			ReserveRatio: 0.3, ToolResultLimit: 1200,
		}),
		agent.WithContentBlockCompaction(),
	)
}
