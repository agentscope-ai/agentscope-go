package formatter

import (
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// OpenAIResponseFormatter formats messages for OpenAI's Responses API.
// Uses input_text/input_image blocks, function_call/function_call_output items,
// and reasoning items with reasoning_item_id.
type OpenAIResponseFormatter struct {
	SupportedInputMediaTypes []string
}

// NewOpenAIResponseFormatter creates a formatter for the OpenAI Responses API.
func NewOpenAIResponseFormatter() *OpenAIResponseFormatter {
	return &OpenAIResponseFormatter{
		SupportedInputMediaTypes: []string{"image/*"},
	}
}

func (f *OpenAIResponseFormatter) Format(msgs []*message.Msg) ([]map[string]any, error) {
	var result []map[string]any
	for _, msg := range msgs {
		if msg == nil || msg.Role == message.RoleSystem {
			continue
		}
		items := f.formatMsg(msg)
		result = append(result, items...)
	}
	return result, nil
}

func (f *OpenAIResponseFormatter) formatMsg(msg *message.Msg) []map[string]any {
	var items []map[string]any
	role := string(msg.Role)

	blocks := msg.GetContentBlocks()
	if len(blocks) == 0 {
		return nil
	}

	var contentParts []map[string]any
	for _, b := range blocks {
		switch blk := b.(type) {
		case message.TextBlock:
			contentParts = append(contentParts, map[string]any{
				"type": "input_text",
				"text": blk.Text,
			})
		case message.ThinkingBlock:
			if itemID, ok := blk.Extra["reasoning_item_id"]; ok && itemID != "" {
				items = append(items, map[string]any{
					"type": "reasoning",
					"id":   itemID,
				})
			}
		case message.ToolCallBlock:
			fc := map[string]any{
				"type":      "function_call",
				"id":        blk.ID,
				"name":      blk.Name,
				"arguments": blk.Input,
			}
			if callID, ok := blk.Extra["call_id"]; ok && callID != "" {
				fc["call_id"] = callID
			} else {
				fc["call_id"] = blk.ID
			}
			items = append(items, fc)
		case message.ToolResultBlock:
			callID := blk.ID
			if blk.Metadata != nil {
				if cid, ok := blk.Metadata["call_id"]; ok {
					if s, ok := cid.(string); ok {
						callID = s
					}
				}
			}
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  ConvertToolResultToString(blk.Output),
			})
		case message.HintBlock:
			contentParts = append(contentParts, map[string]any{
				"type": "input_text",
				"text": blk.GetHintText(),
			})
		case message.DataBlock:
			mt := blk.GetMediaType()
			if SupportsMediaType(f.SupportedInputMediaTypes, mt) {
				if formatted := f.formatResponseDataBlock(blk); formatted != nil {
					contentParts = append(contentParts, formatted)
				}
			}
		}
	}

	if len(contentParts) > 0 {
		items = append([]map[string]any{{
			"role":    role,
			"content": contentParts,
		}}, items...)
	}

	return items
}

func (f *OpenAIResponseFormatter) formatResponseDataBlock(blk message.DataBlock) map[string]any {
	mt := blk.GetMediaType()
	if !SupportsMediaType(f.SupportedInputMediaTypes, mt) {
		return nil
	}
	switch src := blk.Source.(type) {
	case message.Base64Source:
		return map[string]any{
			"type":      "input_image",
			"image_url": fmt.Sprintf("data:%s;base64,%s", src.MediaType, src.Data),
		}
	case message.URLSource:
		return map[string]any{
			"type":      "input_image",
			"image_url": src.URL,
		}
	}
	return nil
}

// FormatMultiAgent formats messages for multi-agent Responses API conversations.
func (f *OpenAIResponseFormatter) FormatMultiAgent(msgs []*message.Msg, currentAgent string) ([]map[string]any, error) {
	return f.Format(msgs)
}

// ExtractResponseInstructions extracts the system message for the Responses API.
func ExtractResponseInstructions(msgs []*message.Msg) string {
	return ExtractSystemPrompt(msgs)
}
