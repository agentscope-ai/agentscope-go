package formatter

import (
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// OpenAIFormatter formats messages for OpenAI-compatible APIs (OpenAI, DashScope, DeepSeek).
type OpenAIFormatter struct {
	// audioAsDataURL wraps base64 audio in a "data:;base64," URL (DashScope).
	audioAsDataURL bool
	// SupportsThinking enables reasoning_content field (DashScope/DeepSeek).
	SupportsThinking bool
	// SupportedInputMediaTypes lists glob patterns of accepted media types (e.g. "image/*").
	SupportedInputMediaTypes []string
	// ToolNameInResult includes the tool name in tool result messages (Ollama requires this).
	ToolNameInResult bool
}

// Format expands each internal message into ordered provider messages. A merged
// agent reply can contain assistant tool calls, tool results and further replies.
func (f *OpenAIFormatter) Format(msgs []*message.Msg) ([]map[string]any, error) {
	var result []map[string]any
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		blocks := msg.GetContentBlocks()
		var segments [][]message.ContentBlock
		start := 0
		for i, block := range blocks {
			if _, ok := block.(message.ToolResultBlock); !ok {
				continue
			}
			if start < i {
				segments = append(segments, blocks[start:i])
			}
			segments = append(segments, blocks[i:i+1])
			start = i + 1
		}
		if start < len(blocks) || len(blocks) == 0 {
			segments = append(segments, blocks[start:])
		}
		for _, segment := range segments {
			// Reuse the provider's block conversion without changing stored history.
			part := *msg
			part.Content = segment
			formatted, err := f.formatMsg(&part)
			if err != nil {
				return nil, err
			}
			if formatted != nil {
				result = append(result, formatted)
			}
		}
	}
	return result, nil
}

func (f *OpenAIFormatter) formatMsg(msg *message.Msg) (map[string]any, error) {
	m := map[string]any{"role": string(msg.Role)}

	blocks := msg.GetContentBlocks()
	if len(blocks) == 0 {
		m["content"] = ""
		return m, nil
	}

	var textParts []string
	var thinkingParts []string
	var toolCalls []map[string]any
	var toolResultID, toolResultContent, toolResultName string
	var multimodalParts []map[string]any
	isToolResult := false
	hasMultimodal := false

	for _, b := range blocks {
		switch blk := b.(type) {
		case message.TextBlock:
			textParts = append(textParts, blk.Text)
		case message.ThinkingBlock:
			thinkingParts = append(thinkingParts, blk.Thinking)
		case message.ToolCallBlock:
			tc := map[string]any{
				"id":   blk.ID,
				"type": "function",
				"function": map[string]any{
					"name":      blk.Name,
					"arguments": blk.Input,
				},
			}
			toolCalls = append(toolCalls, tc)
		case message.ToolResultBlock:
			isToolResult = true
			toolResultID = blk.ID
			toolResultContent = ConvertToolResultToString(blk.Output)
			toolResultName = blk.Name
		case message.HintBlock:
			textParts = append(textParts, blk.GetHintText())
		case message.DataBlock:
			// Use the checked internal variant so invalid audio subtypes
			// surface as Format errors instead of being sent to the API
			// (upstream #2301).
			formatted, err := formatDataBlock(blk, f.SupportedInputMediaTypes, f.audioAsDataURL)
			if err != nil {
				return nil, err
			}
			if formatted != nil {
				multimodalParts = append(multimodalParts, formatted)
				hasMultimodal = true
			}
		}
	}

	if isToolResult {
		result := map[string]any{
			"role":         "tool",
			"tool_call_id": toolResultID,
			"content":      toolResultContent,
		}
		if f.ToolNameInResult {
			result["tool_name"] = toolResultName
		}
		return result, nil
	}

	if len(toolCalls) > 0 {
		m["tool_calls"] = toolCalls
	}

	textContent := joinStrings(textParts)

	if hasMultimodal {
		var contentArray []map[string]any
		if textContent != "" {
			contentArray = append(contentArray, map[string]any{"type": "text", "text": textContent})
		}
		contentArray = append(contentArray, multimodalParts...)
		m["content"] = contentArray
	} else if textContent != "" {
		m["content"] = textContent
	} else if len(toolCalls) > 0 {
		m["content"] = nil
	} else {
		m["content"] = ""
	}

	if f.SupportsThinking && len(thinkingParts) > 0 {
		m["reasoning_content"] = joinStrings(thinkingParts)
	}

	return m, nil
}

func joinStrings(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	result := parts[0]
	for _, p := range parts[1:] {
		result += p
	}
	return result
}

// --- DashScope Formatter (extends OpenAI with video/audio/thinking support) ---

// DashScopeFormatter formats messages for DashScope APIs.
// Extends OpenAI with video_url, input_audio, and reasoning_content support.
type DashScopeFormatter struct {
	OpenAIFormatter
}

// NewDashScopeFormatter creates a formatter for DashScope.
func NewDashScopeFormatter() *DashScopeFormatter {
	return &DashScopeFormatter{
		OpenAIFormatter: OpenAIFormatter{
			audioAsDataURL:           true,
			SupportsThinking:         true,
			SupportedInputMediaTypes: []string{"image/*", "audio/*", "video/*"},
		},
	}
}

// FormatMultiAgent formats messages for multi-agent conversations.
func (f *OpenAIFormatter) FormatMultiAgent(msgs []*message.Msg, currentAgent string) ([]map[string]any, error) {
	return formatMultiAgentOpenAI(f, msgs, currentAgent)
}

// NewOpenAIFormatter creates a formatter for standard OpenAI APIs.
func NewOpenAIFormatter() *OpenAIFormatter {
	return &OpenAIFormatter{
		SupportsThinking:         false,
		SupportedInputMediaTypes: []string{"image/*", "audio/*"},
	}
}

// FormatMultiAgent formats messages for multi-agent DashScope conversations.
func (f *DashScopeFormatter) FormatMultiAgent(msgs []*message.Msg, currentAgent string) ([]map[string]any, error) {
	return formatMultiAgentOpenAI(f, msgs, currentAgent)
}

// --- Anthropic Formatter ---

// AnthropicFormatter formats messages for Anthropic's Messages API.
// Supports image content blocks and ThinkingBlock with signature.
type AnthropicFormatter struct {
	SupportedInputMediaTypes []string
}

func (f *AnthropicFormatter) Format(msgs []*message.Msg) ([]map[string]any, error) {
	var result []map[string]any
	for _, msg := range msgs {
		if msg == nil || msg.Role == message.RoleSystem {
			continue
		}
		result = append(result, f.expandMsg(msg)...)
	}
	return result, nil
}

// expandMsg splits the rendered wire view, leaving stored history untouched.
// Split after filtering empty/unsupported blocks so consecutive tool results
// remain together even when an omitted block separates them internally.
func (f *AnthropicFormatter) expandMsg(msg *message.Msg) []map[string]any {
	formatted := f.formatMsg(msg)
	if formatted == nil {
		return nil
	}
	content := formatted["content"].([]map[string]any)
	var out []map[string]any
	start := 0
	for start < len(content) {
		result := content[start]["type"] == "tool_result"
		end := start + 1
		for end < len(content) && (content[end]["type"] == "tool_result") == result {
			end++
		}
		role := formatted["role"]
		if result {
			role = "user"
		}
		out = append(out, map[string]any{"role": role, "content": content[start:end]})
		start = end
	}
	return out
}

func (f *AnthropicFormatter) formatMsg(msg *message.Msg) map[string]any {
	role := string(msg.Role)
	if role == "tool" {
		role = "user"
	}

	blocks := msg.GetContentBlocks()
	if len(blocks) == 0 {
		return nil
	}

	var content []map[string]any
	for _, b := range blocks {
		switch blk := b.(type) {
		case message.TextBlock:
			if blk.Text != "" {
				content = append(content, map[string]any{
					"type": "text",
					"text": blk.Text,
				})
			}
		case message.ThinkingBlock:
			if blk.Thinking != "" {
				tb := map[string]any{
					"type":     "thinking",
					"thinking": blk.Thinking,
				}
				if sig, ok := blk.Extra["signature"]; ok && sig != "" {
					tb["signature"] = sig
				}
				content = append(content, tb)
			}
		case message.RedactedThinkingBlock:
			rtb := map[string]any{
				"type": "redacted_thinking",
			}
			if blk.Data != "" {
				rtb["data"] = blk.Data
			}
			content = append(content, rtb)
		case message.ToolCallBlock:
			inputObj := jsonLoadsWithRepair(blk.Input)
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    blk.ID,
				"name":  blk.Name,
				"input": inputObj,
			})
		case message.ToolResultBlock:
			resultText := ConvertToolResultToString(blk.Output)
			if resultText == "" {
				resultText = "(empty tool output)"
			}
			trBlock := map[string]any{
				"type":        "tool_result",
				"tool_use_id": blk.ID,
				"content":     resultText,
			}
			content = append(content, trBlock)
		case message.HintBlock:
			for _, sub := range hintContent(blk) {
				switch h := sub.(type) {
				case message.TextBlock:
					content = append(content, map[string]any{"type": "text", "text": h.Text})
				case message.DataBlock:
					if formatted := f.formatAnthropicDataBlock(h); formatted != nil {
						content = append(content, formatted)
					}
				}
			}
		case message.DataBlock:
			if formatted := f.formatAnthropicDataBlock(blk); formatted != nil {
				content = append(content, formatted)
			}
		}
	}

	if len(content) == 0 {
		return nil
	}

	return map[string]any{
		"role":    role,
		"content": content,
	}
}

func (f *AnthropicFormatter) formatAnthropicDataBlock(blk message.DataBlock) map[string]any {
	mt := blk.GetMediaType()
	if !SupportsMediaType(f.SupportedInputMediaTypes, mt) {
		return nil
	}
	switch src := blk.Source.(type) {
	case message.Base64Source:
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": src.MediaType,
				"data":       src.Data,
			},
		}
	case message.URLSource:
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type": "url",
				"url":  src.URL,
			},
		}
	}
	return nil
}

// FormatMultiAgent formats messages for multi-agent Anthropic conversations.
// Uses <history> tags for conversation context from other agents.
func (f *AnthropicFormatter) FormatMultiAgent(msgs []*message.Msg, currentAgent string) ([]map[string]any, error) {
	var formatted []map[string]any
	for _, msg := range msgs {
		if msg == nil || msg.Role == message.RoleSystem {
			continue
		}
		for _, part := range f.expandMsg(msg) {
			content := part["content"].([]map[string]any)
			// A result must lead its user message. Attribute only the sender's own
			// content, never synthetic result segments or original tool messages.
			if (msg.Role == message.RoleUser || msg.Role == message.RoleAssistant) &&
				msg.Name != "" && msg.Name != currentAgent && content[0]["type"] != "tool_result" {
				nameBlock := map[string]any{"type": "text", "text": "[" + msg.Name + "]:"}
				part["content"] = append([]map[string]any{nameBlock}, content...)
			}
			formatted = append(formatted, part)
		}
	}
	return formatted, nil
}

// NewAnthropicFormatter creates a formatter for Anthropic's Messages API.
func NewAnthropicFormatter() *AnthropicFormatter {
	return &AnthropicFormatter{
		SupportedInputMediaTypes: []string{"image/*"},
	}
}

// ExtractSystemPrompt extracts the system message text from a message list.
// Anthropic requires system prompt to be passed separately, not in the messages array.
func ExtractSystemPrompt(msgs []*message.Msg) string {
	for _, msg := range msgs {
		if msg != nil && msg.Role == message.RoleSystem {
			if txt := msg.GetTextContent("\n"); txt != nil {
				return *txt
			}
		}
	}
	return ""
}
