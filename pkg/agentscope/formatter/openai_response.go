package formatter

import (
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"strings"
)

// OpenAIResponseFormatter formats ordered Responses input items, including
// reasoning replay, native tool-result images and provider call identifiers.
type OpenAIResponseFormatter struct {
	SupportedInputMediaTypes []string
}

// NewOpenAIResponseFormatter creates a formatter for Responses input history.
func NewOpenAIResponseFormatter() *OpenAIResponseFormatter {
	return &OpenAIResponseFormatter{SupportedInputMediaTypes: []string{"image/*"}}
}

func (f *OpenAIResponseFormatter) Format(msgs []*message.Msg) ([]map[string]any, error) {
	var result []map[string]any
	callIDs := make(map[string]string)
	for _, msg := range msgs {
		if msg == nil || msg.Role == message.RoleSystem {
			continue
		}
		result = append(result, f.formatMsg(msg, callIDs)...)
	}
	return result, nil
}

func (f *OpenAIResponseFormatter) formatMsg(msg *message.Msg, callIDs map[string]string) []map[string]any {
	role := string(msg.Role)

	blocks := msg.GetContentBlocks()
	if len(blocks) == 0 {
		return []map[string]any{{"role": role, "content": ""}}
	}

	var items []map[string]any
	var content []map[string]any

	flushContent := func() {
		if len(content) == 0 {
			return
		}
		if len(content) == 1 {
			if text, ok := content[0]["text"].(string); ok && content[0]["type"] == "input_text" {
				items = append(items, map[string]any{"role": role, "content": text})
			} else {
				items = append(items, map[string]any{"role": role, "content": content})
			}
		} else {
			items = append(items, map[string]any{"role": role, "content": content})
		}
		content = nil
	}

	for _, b := range blocks {
		switch blk := b.(type) {
		case message.ThinkingBlock:
			raw, ok := blk.Extra["responses_item"].(map[string]any)
			if !ok || len(raw) == 0 {
				// Retain the public formatter's legacy reasoning-item reference.
				if id, valid := blk.Extra["reasoning_item_id"].(string); valid && id != "" {
					flushContent()
					items = append(items, map[string]any{"type": "reasoning", "id": id})
				}
				// A plain thinking block cannot be replayed, and an empty
				// map would serialize to "{}" and get the whole request
				// rejected by the API.
				continue
			}
			flushContent() // reasoning starts a new output segment
			items = append(items, stripResponsesNullsMap(raw))
		case message.TextBlock:
			content = append(content, map[string]any{
				"type": "input_text",
				"text": blk.Text,
			})
		case message.DataBlock:
			if part := f.formatResponseDataBlock(blk); part != nil {
				content = append(content, part)
			}
		case message.HintBlock:
			// Hints are user input, including when attached to an assistant
			// turn. Keep their position between the surrounding segments.
			flushContent()
			switch hint := blk.Hint.(type) {
			case string:
				items = append(items, map[string]any{"role": "user", "content": hint})
			case []message.ContentBlock:
				var parts []map[string]any
				for _, sub := range hint {
					switch h := sub.(type) {
					case message.TextBlock:
						parts = append(parts, map[string]any{"type": "input_text", "text": h.Text})
					case message.DataBlock:
						if part := f.formatResponseDataBlock(h); part != nil {
							parts = append(parts, part)
						}
					}
				}
				if len(parts) > 0 {
					items = append(items, map[string]any{"role": "user", "content": parts})
				}
			}
		case message.ToolCallBlock:
			flushContent()
			callID := responsesCallID(blk.Extra, blk.ID)
			callIDs[blk.ID] = callID
			items = append(items, map[string]any{
				"type":      "function_call",
				"id":        blk.ID,
				"call_id":   callID,
				"name":      blk.Name,
				"arguments": blk.Input,
			})
		case message.ToolResultBlock:
			flushContent()
			callID := blk.ID
			if original, ok := callIDs[blk.ID]; ok {
				callID = original
			}
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": responsesCallID(blk.Metadata, callID),
				"output":  f.formatToolResultOutput(&blk),
			})
		}
	}
	flushContent()
	if len(items) == 0 {
		// Every block was of a type the Responses API cannot carry here
		// (audio, unsupported sources, ...).
		// Emitting nothing would silently drop the turn from the
		// conversation; the old one-item-per-message mapping produced
		// {"role":...,"content":null} and the API rejected it loudly. Keep
		// the turn visible with an explicit note instead of losing it.
		return []map[string]any{{
			"role":    role,
			"content": "[content omitted: this turn carried only block types the Responses API cannot replay]",
		}}
	}
	return items
}

// responsesInputImage preserves image sources without fetching or decoding them.
// Other media keep the adapter's existing omission policy.
func (f *OpenAIResponseFormatter) formatResponseDataBlock(block message.DataBlock) map[string]any {
	if !SupportsMediaType(f.SupportedInputMediaTypes, block.GetMediaType()) {
		return nil
	}
	var url string
	switch src := block.Source.(type) {
	case message.URLSource:
		url = src.URL
	case message.Base64Source:
		url = "data:" + src.MediaType + ";base64," + src.Data
	default:
		return nil
	}
	return map[string]any{"type": "input_image", "image_url": url}
}

// responsesCallID accepts only nonempty provider IDs. Old histories and
// compatible servers without this metadata retain their existing ID fallback.
func responsesCallID(metadata map[string]any, fallback string) string {
	if id, ok := metadata["call_id"].(string); ok && id != "" {
		return id
	}
	return fallback
}

// formatToolResultOutput renders a tool result for the Responses API
// (upstream #2389): when the result carries image data blocks, the output
// becomes native content parts (input_text / input_image) instead of being
// flattened to text; plain results stay a single string.
func (f *OpenAIResponseFormatter) formatToolResultOutput(blk *message.ToolResultBlock) any {
	list, ok := blk.Output.([]message.ContentBlock)
	if !ok {
		return ConvertToolResultToString(blk.Output)
	}
	hasImage := false
	for _, sub := range list {
		if db, ok := sub.(message.DataBlock); ok && strings.HasPrefix(db.GetMediaType(), "image/") && SupportsMediaType(f.SupportedInputMediaTypes, db.GetMediaType()) {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return ConvertToolResultToString(blk.Output)
	}
	parts := make([]map[string]any, 0, len(list))
	for _, sub := range list {
		switch b := sub.(type) {
		case message.TextBlock:
			parts = append(parts, map[string]any{"type": "input_text", "text": b.Text})
		case message.DataBlock:
			if !strings.HasPrefix(b.GetMediaType(), "image/") || !SupportsMediaType(f.SupportedInputMediaTypes, b.GetMediaType()) {
				parts = append(parts, map[string]any{"type": "input_text", "text": ConvertToolResultToString([]message.ContentBlock{b})})
				continue
			}
			switch src := b.Source.(type) {
			case message.URLSource:
				parts = append(parts, map[string]any{"type": "input_image", "image_url": src.URL})
			case message.Base64Source:
				parts = append(parts, map[string]any{
					"type":      "input_image",
					"image_url": "data:" + src.MediaType + ";base64," + src.Data,
				})
			default:
				parts = append(parts, map[string]any{"type": "input_text", "text": ConvertToolResultToString([]message.ContentBlock{b})})
			}
		}
	}
	if len(parts) == 0 {
		return ConvertToolResultToString(blk.Output)
	}
	return parts
}

func stripResponsesNullsMap(m map[string]any) map[string]any {
	out, _ := stripResponsesNulls(m).(map[string]any)
	return out
}

// stripResponsesNulls recursively removes nil-valued map entries so replayed
// items do not carry explicit nulls (upstream #2426: exclude_none — some
// Responses-compatible APIs reject null fields on input items).
func stripResponsesNulls(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if val == nil {
				continue
			}
			out[k] = stripResponsesNulls(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, item := range t {
			out = append(out, stripResponsesNulls(item))
		}
		return out
	default:
		return v
	}
}

// FormatMultiAgent retains the Responses formatter's existing sender policy.
func (f *OpenAIResponseFormatter) FormatMultiAgent(msgs []*message.Msg, currentAgent string) ([]map[string]any, error) {
	return f.Format(msgs)
}

// ExtractResponseInstructions extracts the system message for the Responses API.
func ExtractResponseInstructions(msgs []*message.Msg) string { return ExtractSystemPrompt(msgs) }
