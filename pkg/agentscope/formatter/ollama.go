package formatter

import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"

// OllamaFormatter formats messages for Ollama's OpenAI-compatible API.
type OllamaFormatter struct {
	OpenAIFormatter
}

func NewOllamaFormatter() *OllamaFormatter {
	return &OllamaFormatter{
		OpenAIFormatter: OpenAIFormatter{SupportsThinking: false, ToolNameInResult: true},
	}
}

func (f *OllamaFormatter) FormatMultiAgent(msgs []*message.Msg, currentAgent string) ([]map[string]any, error) {
	return formatMultiAgentOpenAI(f, msgs, currentAgent)
}
