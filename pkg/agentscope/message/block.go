package message

import (
	"encoding/json"
	"strings"
)

// ContentBlockType enumerates supported content block kinds.
type ContentBlockType string

const (
	ContentBlockText             ContentBlockType = "text"
	ContentBlockThinking         ContentBlockType = "thinking"
	ContentBlockRedactedThinking ContentBlockType = "redacted_thinking"
	ContentBlockToolCall         ContentBlockType = "tool_call"
	ContentBlockToolResult       ContentBlockType = "tool_result"
	ContentBlockData             ContentBlockType = "data"
	ContentBlockHint             ContentBlockType = "hint"
)

// ToolCallState tracks the lifecycle of a tool call.
type ToolCallState string

const (
	ToolCallPending   ToolCallState = "pending"
	ToolCallAsking    ToolCallState = "asking"
	ToolCallAllowed   ToolCallState = "allowed"
	ToolCallSubmitted ToolCallState = "submitted"
	ToolCallFinished  ToolCallState = "finished"
)

// ToolResultState tracks the outcome of a tool execution.
type ToolResultState string

const (
	ToolResultSuccess     ToolResultState = "success"
	ToolResultError       ToolResultState = "error"
	ToolResultInterrupted ToolResultState = "interrupted"
	ToolResultDenied      ToolResultState = "denied"
	ToolResultRunning     ToolResultState = "running"
)

// ContentBlock is the common interface for all content blocks.
type ContentBlock interface {
	GetType() ContentBlockType
	GetID() string
}

// TextBlock represents plain text content.
type TextBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Text string `json:"text"`
}

func (b TextBlock) GetType() ContentBlockType { return ContentBlockText }
func (b TextBlock) GetID() string             { return b.ID }

// ThinkingBlock represents internal reasoning content.
type ThinkingBlock struct {
	Type     string         `json:"type"`
	ID       string         `json:"id"`
	Thinking string         `json:"thinking"`
	Extra    map[string]any `json:"extra,omitempty"` // provider-specific fields (e.g. Anthropic "signature", OpenAI Response "reasoning_item_id")
}

func (b ThinkingBlock) GetType() ContentBlockType { return ContentBlockThinking }
func (b ThinkingBlock) GetID() string             { return b.ID }

// RedactedThinkingBlock represents redacted reasoning content from Anthropic.
// The actual thinking content is not available, but the block must be preserved
// in the message round-trip to maintain conversation integrity.
type RedactedThinkingBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Data string `json:"data,omitempty"` // opaque data returned by the API
}

func (b RedactedThinkingBlock) GetType() ContentBlockType { return ContentBlockRedactedThinking }
func (b RedactedThinkingBlock) GetID() string             { return b.ID }

// Base64Source represents base64-encoded binary data.
type Base64Source struct {
	Type      string `json:"type"` // "base64"
	Data      string `json:"data"`
	MediaType string `json:"media_type"`
}

// URLSource represents a URL reference to binary data.
type URLSource struct {
	Type      string `json:"type"` // "url"
	URL       string `json:"url"`
	MediaType string `json:"media_type"`
}

// DataBlock represents binary content (images, audio, video) unified into a
// single block type with media_type discrimination.
type DataBlock struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Source any    `json:"source"` // Base64Source or URLSource
}

func (b DataBlock) GetType() ContentBlockType { return ContentBlockData }
func (b DataBlock) GetID() string             { return b.ID }

// GetMediaType returns the media type from the source.
func (b DataBlock) GetMediaType() string {
	switch s := b.Source.(type) {
	case Base64Source:
		return s.MediaType
	case URLSource:
		return s.MediaType
	case *Base64Source:
		return s.MediaType
	case *URLSource:
		return s.MediaType
	}
	return ""
}

// ToolCallBlock represents a model's request to call a tool.
type ToolCallBlock struct {
	Type           string         `json:"type"`
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Input          string         `json:"input"` // raw JSON string of arguments
	State          ToolCallState  `json:"state"`
	SuggestedRules []any          `json:"suggested_rules,omitempty"`
	Extra          map[string]any `json:"extra,omitempty"` // provider-specific fields (e.g. OpenAI Response "call_id")
}

func (b ToolCallBlock) GetType() ContentBlockType { return ContentBlockToolCall }
func (b ToolCallBlock) GetID() string             { return b.ID }

// ParseInput parses the raw JSON input into a map.
func (b ToolCallBlock) ParseInput() (map[string]any, error) {
	var result map[string]any
	if b.Input == "" {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal([]byte(b.Input), &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ToolResultBlock represents the result of a tool execution.
type ToolResultBlock struct {
	Type     string          `json:"type"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Output   any             `json:"output"` // string or []ContentBlock
	State    ToolResultState `json:"state"`
	Metadata map[string]any  `json:"metadata,omitempty"`
}

func (b ToolResultBlock) GetType() ContentBlockType { return ContentBlockToolResult }
func (b ToolResultBlock) GetID() string             { return b.ID }

// GetOutputText returns the text content of the tool result output.
func (b ToolResultBlock) GetOutputText() string {
	switch o := b.Output.(type) {
	case string:
		return o
	case []ContentBlock:
		var texts []string
		for _, block := range o {
			if tb, ok := block.(TextBlock); ok {
				texts = append(texts, tb.Text)
			}
		}
		if len(texts) > 0 {
			return texts[0]
		}
	}
	return ""
}

// HintBlock represents instructions or hints injected by middleware.
// Hint can be a string or []ContentBlock (for multimodal hints).
type HintBlock struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Source string `json:"source,omitempty"`
	Hint   any    `json:"hint"` // string or []ContentBlock
}

func (b HintBlock) GetType() ContentBlockType { return ContentBlockHint }
func (b HintBlock) GetID() string             { return b.ID }

// GetHintText returns the hint as a plain string.
// If Hint is a []ContentBlock, it concatenates all TextBlock texts.
func (b HintBlock) GetHintText() string {
	switch h := b.Hint.(type) {
	case string:
		return h
	case []ContentBlock:
		var texts []string
		for _, block := range h {
			if tb, ok := block.(TextBlock); ok {
				texts = append(texts, tb.Text)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}
	return ""
}

// MarshalJSON handles the polymorphic Hint field.
func (b HintBlock) MarshalJSON() ([]byte, error) {
	type Alias struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		Source string `json:"source,omitempty"`
		Hint   any    `json:"hint"`
	}
	return json.Marshal(Alias(b))
}

// UnmarshalJSON handles the polymorphic Hint field.
func (b *HintBlock) UnmarshalJSON(data []byte) error {
	type Alias struct {
		Type   string          `json:"type"`
		ID     string          `json:"id"`
		Source string          `json:"source,omitempty"`
		Hint   json.RawMessage `json:"hint"`
	}
	var a Alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	b.Type = a.Type
	b.ID = a.ID
	b.Source = a.Source

	if len(a.Hint) == 0 {
		b.Hint = ""
		return nil
	}

	// Try string first
	var s string
	if err := json.Unmarshal(a.Hint, &s); err == nil {
		b.Hint = s
		return nil
	}

	// Try []ContentBlock
	blocks, err := UnmarshalContentBlocks(a.Hint)
	if err == nil {
		b.Hint = blocks
		return nil
	}

	// Fallback to raw string
	b.Hint = string(a.Hint)
	return nil
}
