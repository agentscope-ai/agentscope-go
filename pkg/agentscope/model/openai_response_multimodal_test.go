package model

import (
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Upstream #2389: tool results carrying image data must reach the Responses
// API as native content parts instead of being flattened to text.
func TestFormatInputItemsNativeMultimodalToolOutput(t *testing.T) {
	m := newTestResponseModel()
	msg := &message.Msg{
		Role: message.RoleUser,
		Name: "u",
		Content: []message.ContentBlock{
			message.ToolResultBlock{
				Type: "tool_result", ID: "c1", Name: "screenshot",
				Output: []message.ContentBlock{
					message.TextBlock{Type: "text", Text: "here is the screen:"},
					message.DataBlock{
						Type: "data", ID: "d1",
						Source: message.Base64Source{Type: "base64", Data: "QUJD", MediaType: "image/png"},
					},
				},
				State: message.ToolResultSuccess,
			},
		},
	}
	items := m.formatInputItems(msg)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	parts, ok := items[0]["output"].([]map[string]any)
	if !ok {
		t.Fatalf("output = %T, want native content parts", items[0]["output"])
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 (text + image)", len(parts))
	}
	if parts[0]["type"] != "input_text" || parts[0]["text"] != "here is the screen:" {
		t.Errorf("parts[0] = %v", parts[0])
	}
	if parts[1]["type"] != "input_image" || parts[1]["image_url"] != "data:image/png;base64,QUJD" {
		t.Errorf("parts[1] = %v, want input_image data URL", parts[1])
	}
}

func TestFormatInputItemsURLImageToolOutput(t *testing.T) {
	m := newTestResponseModel()
	msg := &message.Msg{
		Role: message.RoleUser,
		Name: "u",
		Content: []message.ContentBlock{
			message.ToolResultBlock{
				Type: "tool_result", ID: "c1", Name: "fetch",
				Output: []message.ContentBlock{
					message.DataBlock{Type: "data", ID: "d1",
						Source: message.URLSource{Type: "url", URL: "https://cdn/i.png", MediaType: "image/png"}},
				},
				State: message.ToolResultSuccess,
			},
		},
	}
	items := m.formatInputItems(msg)
	parts := items[0]["output"].([]map[string]any)
	if parts[0]["image_url"] != "https://cdn/i.png" {
		t.Errorf("URL image part = %v", parts[0])
	}
}

// Plain-text tool results keep the single-string output shape (no churn for
// the common case).
func TestFormatInputItemsTextToolOutputUnchanged(t *testing.T) {
	m := newTestResponseModel()
	msg := &message.Msg{
		Role: message.RoleUser,
		Name: "u",
		Content: []message.ContentBlock{
			message.ToolResultBlock{Type: "tool_result", ID: "c1", Name: "read", Output: "plain text", State: message.ToolResultSuccess},
			message.ToolResultBlock{Type: "tool_result", ID: "c2", Name: "ls",
				Output: []message.ContentBlock{message.TextBlock{Type: "text", Text: "a\nb"}}, State: message.ToolResultSuccess},
		},
	}
	items := m.formatInputItems(msg)
	if s, ok := items[0]["output"].(string); !ok || s != "plain text" {
		t.Errorf("string output = %v, want unchanged string", items[0]["output"])
	}
	if s, ok := items[1]["output"].(string); !ok || s != "a\nb" {
		t.Errorf("text-only block list output = %v, want flattened string", items[1]["output"])
	}
}
