package formatter

import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"

// MoonshotFormatter formats messages for Moonshot/Kimi's OpenAI-compatible API.
type MoonshotFormatter struct {
	OpenAIFormatter
}

func NewMoonshotFormatter() *MoonshotFormatter {
	return &MoonshotFormatter{
		OpenAIFormatter: OpenAIFormatter{SupportsThinking: false},
	}
}

func (f *MoonshotFormatter) FormatMultiAgent(msgs []*message.Msg, currentAgent string) ([]map[string]any, error) {
	return formatMultiAgentOpenAI(f, msgs, currentAgent)
}
