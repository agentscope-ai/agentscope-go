package formatter

import (
	"fmt"
	"path"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Formatter transforms internal Msg objects into provider-specific API payloads.
type Formatter interface {
	// Format converts messages into the format expected by a model provider's API.
	Format(msgs []*message.Msg) ([]map[string]any, error)
}

// MultiAgentFormatter extends Formatter with multi-agent support.
// It handles injecting agent names, merging consecutive same-role messages,
// and system prompt interleaving for multi-agent conversations.
type MultiAgentFormatter interface {
	Formatter
	FormatMultiAgent(msgs []*message.Msg, currentAgent string) ([]map[string]any, error)
}

// SupportsMediaType checks if a media type is supported by the formatter.
// Uses path.Match for glob-style matching (e.g. "image/*" matches "image/png").
func SupportsMediaType(supported []string, mediaType string) bool {
	for _, pattern := range supported {
		if matched, _ := path.Match(pattern, mediaType); matched {
			return true
		}
	}
	return false
}

// ConvertToolResultToString converts a tool result output to a string.
// Handles string, []ContentBlock, and other types.
func ConvertToolResultToString(output any) string {
	switch o := output.(type) {
	case string:
		return o
	case []message.ContentBlock:
		var parts []string
		for _, b := range o {
			if tb, ok := b.(message.TextBlock); ok {
				parts = append(parts, tb.Text)
			} else if db, ok := b.(message.DataBlock); ok {
				mt := db.GetMediaType()
				if len(mt) == 0 || len(mt) > 64 || strings.ContainsAny(mt, "\r\n") {
					mt = "media"
				}
				// URLs may themselves contain inline data. Only retain short,
				// ordinary URLs in a text-only placeholder.
				src, ok := db.Source.(message.URLSource)
				if ok && len(src.URL) <= 256 && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(src.URL)), "data:") {
					parts = append(parts, fmt.Sprintf("[%s: %s]", mt, src.URL))
				} else {
					parts = append(parts, fmt.Sprintf("[%s data]", mt))
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprintf("%v", o)
	}
}

// MessageGroup represents a group of messages classified by type.
type MessageGroup struct {
	Type string         // "tool_sequence" or "agent_message"
	Msgs []*message.Msg // messages in this group
}

// GroupMessages separates messages into tool sequences and agent messages.
// A tool sequence is a contiguous run of messages containing tool_call or tool_result blocks.
func GroupMessages(msgs []*message.Msg) []MessageGroup {
	var groups []MessageGroup
	var currentGroup *MessageGroup

	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		isToolMsg := msg.HasContentBlocks(message.ContentBlockToolCall, message.ContentBlockToolResult)

		groupType := "agent_message"
		if isToolMsg {
			groupType = "tool_sequence"
		}

		if currentGroup == nil || currentGroup.Type != groupType {
			groups = append(groups, MessageGroup{Type: groupType})
			currentGroup = &groups[len(groups)-1]
		}
		currentGroup.Msgs = append(currentGroup.Msgs, msg)
	}
	return groups
}

// audioFormatMap is the explicit media-type → input_audio format mapping
// (upstream #2301). OpenAI-compatible input_audio APIs only accept wav|mp3
// format labels; anything else must be rejected with a clear error instead
// of being passed through for the API to refuse.
var audioFormatMap = map[string]string{
	"wav":  "wav",
	"mp3":  "mp3",
	"mpeg": "mp3", // audio/mpeg IS mp3 audio
}

// FormatDataBlockForOpenAI converts a DataBlock to OpenAI image_url format.
// Returns nil when the block is unsupported or carries an invalid audio
// subtype; use OpenAIFormatter.Format to surface the latter as an error.
func FormatDataBlockForOpenAI(blk message.DataBlock, supported []string) map[string]any {
	formatted, _ := formatDataBlock(blk, supported, false)
	return formatted
}

// FormatDataBlockForDashScope converts a DataBlock for DashScope's compatible
// API: like OpenAI, but base64 audio is wrapped in a data URL and the mpeg
// format suffix maps to mp3 (upstream fix #2315).
func FormatDataBlockForDashScope(blk message.DataBlock, supported []string) map[string]any {
	formatted, _ := formatDataBlock(blk, supported, true)
	return formatted
}

// formatDataBlock converts a DataBlock for OpenAI-style APIs. It returns
// (nil, nil) for unsupported media, but an error for malformed input the
// API would reject (upstream #2301: unknown audio subtypes).
func formatDataBlock(blk message.DataBlock, supported []string, audioDataURL bool) (map[string]any, error) {
	mt := blk.GetMediaType()
	if !SupportsMediaType(supported, mt) {
		return nil, nil
	}
	if strings.HasPrefix(mt, "image/") {
		switch src := blk.Source.(type) {
		case message.Base64Source:
			return map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": fmt.Sprintf("data:%s;base64,%s", src.MediaType, src.Data),
				},
			}, nil
		case message.URLSource:
			return map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": src.URL,
				},
			}, nil
		}
	}
	if strings.HasPrefix(mt, "audio/") {
		if src, ok := blk.Source.(message.Base64Source); ok {
			subtype := strings.TrimPrefix(src.MediaType, "audio/")
			format, known := audioFormatMap[subtype]
			if !known {
				return nil, fmt.Errorf(
					"formatter: unsupported audio media type %q (supported: audio/wav, audio/mp3, audio/mpeg)",
					src.MediaType)
			}
			data := src.Data
			if audioDataURL {
				// DashScope requires base64 audio wrapped in a data URL.
				data = "data:;base64," + src.Data
			}
			return map[string]any{
				"type": "input_audio",
				"input_audio": map[string]any{
					"data":   data,
					"format": format,
				},
			}, nil
		}
	}
	if strings.HasPrefix(mt, "video/") {
		switch src := blk.Source.(type) {
		case message.URLSource:
			return map[string]any{
				"type": "video_url",
				"video_url": map[string]any{
					"url": src.URL,
				},
			}, nil
		case message.Base64Source:
			return map[string]any{
				"type": "video_url",
				"video_url": map[string]any{
					"url": fmt.Sprintf("data:%s;base64,%s", src.MediaType, src.Data),
				},
			}, nil
		}
	}
	return nil, nil
}

// formatMultiAgentOpenAI applies multi-agent formatting for OpenAI-compatible APIs:
// injects "name" field on user/assistant messages, merges consecutive same-role
// messages with name prefixes.
func formatMultiAgentOpenAI(base Formatter, msgs []*message.Msg, currentAgent string) ([]map[string]any, error) {
	var formatted []map[string]any
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		parts, err := base.Format([]*message.Msg{msg})
		if err != nil {
			return nil, err
		}
		// One internal message may expand into several wire messages. Assign
		// its sender before combining results, rather than matching by index.
		for _, part := range parts {
			role, _ := part["role"].(string)
			if (role == "user" || role == "assistant") && msg.Name != "" && msg.Name != currentAgent {
				part["name"] = sanitizeName(msg.Name)
			}
		}
		formatted = append(formatted, parts...)
	}

	return mergeConsecutiveRoles(formatted), nil
}

func mergeConsecutiveRoles(msgs []map[string]any) []map[string]any {
	if len(msgs) <= 1 {
		return msgs
	}
	var result []map[string]any
	for _, m := range msgs {
		if len(result) > 0 {
			prev := result[len(result)-1]
			prevRole, _ := prev["role"].(string)
			curRole, _ := m["role"].(string)
			prevContent, prevPlain := prev["content"].(string)
			curContent, curPlain := m["content"].(string)
			// Text concatenation cannot preserve tool calls, reasoning or media.
			if prevRole == curRole && curRole != "tool" && curRole != "system" &&
				prevPlain && curPlain && prev["tool_calls"] == nil && m["tool_calls"] == nil &&
				prev["reasoning_content"] == nil && m["reasoning_content"] == nil {
				name, _ := m["name"].(string)
				if name != "" {
					curContent = "[" + name + "]: " + curContent
				}
				prev["content"] = prevContent + "\n" + curContent
				continue
			}
		}
		result = append(result, m)
	}
	return result
}

func sanitizeName(name string) string {
	r := strings.NewReplacer(" ", "_", "-", "_", ".", "_")
	return r.Replace(name)
}
