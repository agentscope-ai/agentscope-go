package model

import (
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/formatter"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func (m *openaiResponseModel) formatInputItems(msg *message.Msg) []map[string]any {
	items, err := formatter.NewOpenAIResponseFormatter().Format([]*message.Msg{msg})
	if err != nil {
		panic(err)
	}
	return items
}
