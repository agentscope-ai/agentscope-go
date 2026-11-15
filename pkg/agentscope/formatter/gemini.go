package formatter

import (
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// GeminiFormatter formats messages for Google Gemini's native API.
// Gemini uses a different structure: role is "user"/"model", content is a
// "parts" array containing text, functionCall, and functionResponse objects.
type GeminiFormatter struct{}

func NewGeminiFormatter() *GeminiFormatter {
	return &GeminiFormatter{}
}

func (f *GeminiFormatter) Format(msgs []*message.Msg) ([]map[string]any, error) {
	var result []map[string]any
	for _, msg := range msgs {
		if msg == nil || msg.Role == message.RoleSystem {
			continue
		}
		formatted := f.formatMsg(msg)
		if formatted != nil {
			result = append(result, formatted)
		}
	}
	return result, nil
}

func (f *GeminiFormatter) FormatMultiAgent(msgs []*message.Msg, currentAgent string) ([]map[string]any, error) {
	var formatted []map[string]any
	for _, msg := range msgs {
		if msg == nil || msg.Role == message.RoleSystem {
			continue
		}
		m := f.formatMsg(msg)
		if m == nil {
			continue
		}
		if msg.Name != "" && msg.Name != currentAgent {
			parts, _ := m["parts"].([]map[string]any)
			namePrefix := map[string]any{"text": "[" + msg.Name + "]: "}
			m["parts"] = append([]map[string]any{namePrefix}, parts...)
		}
		formatted = append(formatted, m)
	}
	return formatted, nil
}

func (f *GeminiFormatter) formatMsg(msg *message.Msg) map[string]any {
	role := "user"
	if msg.Role == message.RoleAssistant {
		role = "model"
	}

	blocks := msg.GetContentBlocks()
	if len(blocks) == 0 {
		return nil
	}

	var parts []map[string]any
	for _, b := range blocks {
		switch blk := b.(type) {
		case message.TextBlock:
			if blk.Text != "" {
				parts = append(parts, map[string]any{"text": blk.Text})
			}
		case message.ThinkingBlock:
			if blk.Thinking != "" {
				parts = append(parts, map[string]any{
					"thought": true,
					"text":    blk.Thinking,
				})
			}
		case message.ToolCallBlock:
			args := jsonLoadsWithRepair(blk.Input)
			parts = append(parts, map[string]any{
				"functionCall": map[string]any{
					"name": blk.Name,
					"args": args,
				},
			})
		case message.ToolResultBlock:
			parts = append(parts, map[string]any{
				"functionResponse": map[string]any{
					"name":     blk.Name,
					"response": map[string]any{"result": ConvertToolResultToString(blk.Output)},
				},
			})
		case message.HintBlock:
			for _, sub := range hintContent(blk) {
				switch h := sub.(type) {
				case message.TextBlock:
					parts = append(parts, map[string]any{"text": h.Text})
				case message.DataBlock:
					if formatted := formatGeminiDataBlock(h); formatted != nil {
						parts = append(parts, formatted)
					}
				}
			}
		case message.DataBlock:
			if formatted := formatGeminiDataBlock(blk); formatted != nil {
				parts = append(parts, formatted)
			}
		}
	}

	if len(parts) == 0 {
		return nil
	}

	return map[string]any{
		"role":  role,
		"parts": parts,
	}
}

func formatGeminiDataBlock(blk message.DataBlock) map[string]any {
	mt := blk.GetMediaType()
	switch src := blk.Source.(type) {
	case message.Base64Source:
		return map[string]any{
			"inlineData": map[string]any{
				"mimeType": mt,
				"data":     src.Data,
			},
		}
	case message.URLSource:
		return map[string]any{
			"fileData": map[string]any{
				"mimeType": mt,
				"fileUri":  src.URL,
			},
		}
	}
	return nil
}

// ExtractGeminiSystemInstruction extracts the system message for Gemini.
// Gemini passes system instructions separately via systemInstruction field.
func ExtractGeminiSystemInstruction(msgs []*message.Msg) map[string]any {
	for _, msg := range msgs {
		if msg != nil && msg.Role == message.RoleSystem {
			var texts []string
			for _, block := range msg.GetContentBlocks(message.ContentBlockText) {
				if text, ok := block.(message.TextBlock); ok && text.Text != "" {
					texts = append(texts, text.Text)
				}
			}
			if len(texts) > 0 {
				return map[string]any{
					"parts": []map[string]any{{"text": strings.Join(texts, "\n")}},
				}
			}
		}
	}
	return nil
}
