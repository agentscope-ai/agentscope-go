package message

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// TimestampFormat is the canonical timestamp layout used throughout agentscope-go.
const TimestampFormat = "2006-01-02 15:04:05.000"

// Role represents the sender role of a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// Usage tracks token consumption for a message.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheInputTokens         int `json:"cache_input_tokens,omitempty"`
}

// Msg is the core message type in agentscope-go.
// Content is always []ContentBlock internally.
type Msg struct {
	ID         string           `json:"id"`
	Name       string           `json:"name"`
	Role       Role             `json:"role"`
	Content    []ContentBlock   `json:"content"`
	Metadata   types.JSONObject `json:"metadata,omitempty"`
	Timestamp  string           `json:"timestamp"`
	FinishedAt string           `json:"finished_at,omitempty"`
	Usage      *Usage           `json:"usage,omitempty"`
}

// MarshalJSON serializes Msg to JSON, handling the polymorphic ContentBlock slice.
func (m *Msg) MarshalJSON() ([]byte, error) {
	type msgAlias Msg
	return json.Marshal(&struct {
		*msgAlias
		Content []ContentBlock `json:"content"`
	}{
		msgAlias: (*msgAlias)(m),
		Content:  m.Content,
	})
}

// UnmarshalJSON deserializes Msg from JSON, reconstructing typed ContentBlock slices.
func (m *Msg) UnmarshalJSON(data []byte) error {
	type msgAlias Msg
	raw := struct {
		*msgAlias
		Content json.RawMessage `json:"content"`
	}{
		msgAlias: (*msgAlias)(m),
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	if len(raw.Content) == 0 {
		return nil
	}

	blocks, err := UnmarshalContentBlocks(raw.Content)
	if err != nil {
		return fmt.Errorf("unmarshal content blocks: %w", err)
	}
	m.Content = blocks
	return nil
}

// UnmarshalContentBlocks deserializes a JSON array into typed ContentBlock slices.
func UnmarshalContentBlocks(data json.RawMessage) ([]ContentBlock, error) {
	var rawBlocks []json.RawMessage
	if err := json.Unmarshal(data, &rawBlocks); err != nil {
		return nil, err
	}

	blocks := make([]ContentBlock, 0, len(rawBlocks))
	for _, raw := range rawBlocks {
		var peek struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &peek); err != nil {
			return nil, err
		}

		var block ContentBlock
		switch ContentBlockType(peek.Type) {
		case ContentBlockText:
			var b TextBlock
			if err := json.Unmarshal(raw, &b); err != nil {
				return nil, err
			}
			block = b
		case ContentBlockThinking:
			var b ThinkingBlock
			if err := json.Unmarshal(raw, &b); err != nil {
				return nil, err
			}
			block = b
		case ContentBlockRedactedThinking:
			var b RedactedThinkingBlock
			if err := json.Unmarshal(raw, &b); err != nil {
				return nil, err
			}
			block = b
		case ContentBlockToolCall:
			var b ToolCallBlock
			if err := json.Unmarshal(raw, &b); err != nil {
				return nil, err
			}
			block = b
		case ContentBlockToolResult:
			var b toolResultBlockJSON
			if err := json.Unmarshal(raw, &b); err != nil {
				return nil, err
			}
			block = b.toBlock()
		case ContentBlockData:
			var b dataBlockJSON
			if err := json.Unmarshal(raw, &b); err != nil {
				return nil, err
			}
			blk, err := b.toBlock()
			if err != nil {
				return nil, err
			}
			block = blk
		case ContentBlockHint:
			var b HintBlock
			if err := json.Unmarshal(raw, &b); err != nil {
				return nil, err
			}
			block = b
		default:
			var b TextBlock
			if err := json.Unmarshal(raw, &b); err != nil {
				return nil, err
			}
			block = b
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

type toolResultBlockJSON struct {
	Type     string          `json:"type"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Output   json.RawMessage `json:"output"`
	State    ToolResultState `json:"state"`
	Metadata map[string]any  `json:"metadata,omitempty"`
}

func (b toolResultBlockJSON) toBlock() ToolResultBlock {
	var output any
	switch {
	case len(b.Output) == 0:
		output = ""
	default:
		// Try a plain string first (the common case).
		var s string
		if err := json.Unmarshal(b.Output, &s); err == nil {
			output = s
		} else if blocks, err := UnmarshalContentBlocks(b.Output); err == nil {
			// Structured tool output (e.g. text + image) must round-trip as
			// []ContentBlock, not degrade into a raw JSON string.
			output = blocks
		} else {
			output = string(b.Output)
		}
	}
	return ToolResultBlock{
		Type:     b.Type,
		ID:       b.ID,
		Name:     b.Name,
		Output:   output,
		State:    b.State,
		Metadata: b.Metadata,
	}
}

type dataBlockJSON struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Name   string          `json:"name,omitempty"`
	Source json.RawMessage `json:"source"`
}

func (b dataBlockJSON) toBlock() (DataBlock, error) {
	var peek struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b.Source, &peek); err != nil {
		return DataBlock{}, err
	}

	var source any
	switch peek.Type {
	case "base64":
		var s Base64Source
		if err := json.Unmarshal(b.Source, &s); err != nil {
			return DataBlock{}, err
		}
		source = s
	case "url":
		var s URLSource
		if err := json.Unmarshal(b.Source, &s); err != nil {
			return DataBlock{}, err
		}
		source = s
	default:
		source = map[string]any{"raw": string(b.Source)}
	}

	return DataBlock{
		Type:   b.Type,
		ID:     b.ID,
		Name:   b.Name,
		Source: source,
	}, nil
}

// NewMsg constructs a new Msg with generated ID and timestamp.
// Content accepts string (auto-wrapped into TextBlock) or []ContentBlock.
// Any other type causes a panic.
func NewMsg(name string, role Role, content any) *Msg {
	var blocks []ContentBlock
	switch c := content.(type) {
	case string:
		blocks = []ContentBlock{TextBlock{Type: "text", ID: agentscope.GenerateID(), Text: c}}
	case []ContentBlock:
		blocks = c
	default:
		panic(fmt.Sprintf("message: content must be string or []ContentBlock, got %T", content))
	}

	return &Msg{
		ID:        agentscope.GenerateID(),
		Name:      name,
		Role:      role,
		Content:   blocks,
		Metadata:  types.JSONObject{},
		Timestamp: time.Now().Format(TimestampFormat),
	}
}

// UserMsg creates a user-role message. Content must contain only TextBlock or DataBlock.
func UserMsg(name string, content any) *Msg {
	msg := NewMsg(name, RoleUser, content)
	msg.FinishedAt = msg.Timestamp
	for _, b := range msg.Content {
		t := b.GetType()
		if t != ContentBlockText && t != ContentBlockData {
			panic(fmt.Sprintf("message: user message may only contain TextBlock or DataBlock, got %s", t))
		}
	}
	return msg
}

// AssistantMsg creates an assistant-role message. Accepts any ContentBlock type.
func AssistantMsg(name string, content any) *Msg {
	return NewMsg(name, RoleAssistant, content)
}

// SystemMsg creates a system-role message. Content must contain only TextBlock.
func SystemMsg(name string, content any) *Msg {
	msg := NewMsg(name, RoleSystem, content)
	msg.FinishedAt = msg.Timestamp
	for _, b := range msg.Content {
		if b.GetType() != ContentBlockText {
			panic(fmt.Sprintf("message: system message may only contain TextBlock, got %s", b.GetType()))
		}
	}
	return msg
}

// GetTextContent returns concatenated text from all text blocks.
func (m *Msg) GetTextContent(separator string) *string {
	if m == nil || len(m.Content) == 0 {
		return nil
	}
	var texts []string
	for _, b := range m.Content {
		if b.GetType() == ContentBlockText {
			if tb, ok := b.(TextBlock); ok {
				texts = append(texts, tb.Text)
			}
		}
	}
	if len(texts) == 0 {
		return nil
	}
	joined := strings.Join(texts, separator)
	return &joined
}

// HasContentBlocks reports whether the message contains content blocks,
// optionally filtered by type.
func (m *Msg) HasContentBlocks(targetTypes ...ContentBlockType) bool {
	return len(m.GetContentBlocks(targetTypes...)) > 0
}

// GetContentBlocks returns all content blocks, optionally filtered by types.
func (m *Msg) GetContentBlocks(targetTypes ...ContentBlockType) []ContentBlock {
	if m == nil || len(m.Content) == 0 {
		return nil
	}
	if len(targetTypes) == 0 {
		return m.Content
	}
	filter := make(map[ContentBlockType]struct{}, len(targetTypes))
	for _, t := range targetTypes {
		filter[t] = struct{}{}
	}
	var out []ContentBlock
	for _, b := range m.Content {
		if _, ok := filter[b.GetType()]; ok {
			out = append(out, b)
		}
	}
	return out
}

// ToMap converts message to a generic map representation.
func (m *Msg) ToMap() map[string]any {
	if m == nil {
		return nil
	}
	result := map[string]any{
		"id":        m.ID,
		"name":      m.Name,
		"role":      string(m.Role),
		"content":   m.Content,
		"metadata":  m.Metadata,
		"timestamp": m.Timestamp,
	}
	if m.FinishedAt != "" {
		result["finished_at"] = m.FinishedAt
	}
	if m.Usage != nil {
		result["usage"] = m.Usage
	}
	return result
}

// findBlockByTypeAndID locates a block in Content by type and ID, returning its index.
func (m *Msg) findBlockByTypeAndID(blockType ContentBlockType, blockID string) int {
	for i, b := range m.Content {
		if b.GetType() == blockType && b.GetID() == blockID {
			return i
		}
	}
	return -1
}
