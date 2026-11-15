package formatter

import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"

// XAIFormatter formats messages for xAI/Grok's OpenAI-compatible API.
type XAIFormatter struct {
	OpenAIFormatter
}

func NewXAIFormatter() *XAIFormatter {
	return &XAIFormatter{
		OpenAIFormatter: OpenAIFormatter{SupportsThinking: false},
	}
}

func (f *XAIFormatter) FormatMultiAgent(msgs []*message.Msg, currentAgent string) ([]map[string]any, error) {
	return formatMultiAgentOpenAI(f, msgs, currentAgent)
}
