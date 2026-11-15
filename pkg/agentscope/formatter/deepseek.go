package formatter

import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"

// DeepSeekFormatter formats messages for DeepSeek APIs.
// DeepSeek uses OpenAI-compatible format with reasoning_content support.
type DeepSeekFormatter struct {
	OpenAIFormatter
}

func NewDeepSeekFormatter() *DeepSeekFormatter {
	return &DeepSeekFormatter{
		OpenAIFormatter: OpenAIFormatter{SupportsThinking: true},
	}
}

func (f *DeepSeekFormatter) FormatMultiAgent(msgs []*message.Msg, currentAgent string) ([]map[string]any, error) {
	return formatMultiAgentOpenAI(f, msgs, currentAgent)
}
