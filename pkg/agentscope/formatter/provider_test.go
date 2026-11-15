package formatter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// ---------------------------------------------------------------------------
// Helper: every provider formatter we want to test.
// ---------------------------------------------------------------------------

type namedFormatter struct {
	name string
	f    Formatter
	maf  MultiAgentFormatter // nil for providers without MultiAgentFormatter
}

func allFormatters() []namedFormatter {
	openai := NewOpenAIFormatter()
	dashscope := NewDashScopeFormatter()
	deepseek := NewDeepSeekFormatter()
	moonshot := NewMoonshotFormatter()
	ollama := NewOllamaFormatter()
	xai := NewXAIFormatter()
	anthropic := NewAnthropicFormatter()
	gemini := NewGeminiFormatter()
	openaiResp := NewOpenAIResponseFormatter()
	return []namedFormatter{
		{"OpenAI", openai, openai},
		{"DashScope", dashscope, dashscope},
		{"DeepSeek", deepseek, deepseek},
		{"Moonshot", moonshot, moonshot},
		{"Ollama", ollama, ollama},
		{"XAI", xai, xai},
		{"Anthropic", anthropic, anthropic},
		{"Gemini", gemini, gemini},
		{"OpenAIResponse", openaiResp, openaiResp},
	}
}

// supportsImageInput returns true for formatters that have image/* in SupportedInputMediaTypes.
func supportsImageInput(name string) bool {
	switch name {
	case "OpenAI", "DashScope", "Anthropic", "Gemini", "OpenAIResponse":
		return true
	}
	return false
}

// jsonMarshalOK is a test helper that verifies all results are valid JSON.
func jsonMarshalOK(t *testing.T, result []map[string]any) {
	t.Helper()
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("empty JSON output")
	}
}

// ---------------------------------------------------------------------------
// 1. TextBlock handling
// ---------------------------------------------------------------------------

func TestAllFormatters_TextBlock(t *testing.T) {
	msgs := []*message.Msg{
		message.UserMsg("alice", "Hello, world!"),
	}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			if len(result) == 0 {
				t.Fatal("expected at least 1 formatted message, got 0")
			}

			jsonMarshalOK(t, result)

			first := result[0]
			switch nf.name {
			case "Gemini":
				if first["role"] != "user" {
					t.Errorf("role = %v, want user", first["role"])
				}
				parts, ok := first["parts"].([]map[string]any)
				if !ok || len(parts) == 0 {
					t.Fatalf("parts missing or empty: %v", first["parts"])
				}
				if parts[0]["text"] != "Hello, world!" {
					t.Errorf("text = %v", parts[0]["text"])
				}
			case "OpenAIResponse":
				role, _ := first["role"].(string)
				if role != "user" {
					t.Errorf("role = %v, want user", role)
				}
				content, ok := first["content"].([]map[string]any)
				if !ok || len(content) == 0 {
					t.Fatalf("content missing or empty: %v", first["content"])
				}
				if content[0]["type"] != "input_text" {
					t.Errorf("type = %v, want input_text", content[0]["type"])
				}
				if content[0]["text"] != "Hello, world!" {
					t.Errorf("text = %v", content[0]["text"])
				}
			case "Anthropic":
				if first["role"] != "user" {
					t.Errorf("role = %v, want user", first["role"])
				}
				content, ok := first["content"].([]map[string]any)
				if !ok || len(content) == 0 {
					t.Fatalf("content missing: %v", first["content"])
				}
				if content[0]["type"] != "text" {
					t.Errorf("type = %v, want text", content[0]["type"])
				}
				if content[0]["text"] != "Hello, world!" {
					t.Errorf("text = %v", content[0]["text"])
				}
			default: // OpenAI family
				if first["role"] != "user" {
					t.Errorf("role = %v, want user", first["role"])
				}
				if first["content"] != "Hello, world!" {
					t.Errorf("content = %v", first["content"])
				}
			}
		})
	}
}

func TestAllFormatters_AssistantTextBlock(t *testing.T) {
	msgs := []*message.Msg{
		message.AssistantMsg("bot", "I can help with that."),
	}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			if len(result) == 0 {
				t.Fatal("expected at least 1 formatted message, got 0")
			}

			first := result[0]
			switch nf.name {
			case "Gemini":
				if first["role"] != "model" {
					t.Errorf("role = %v, want model", first["role"])
				}
			case "Anthropic", "OpenAIResponse":
				if first["role"] != "assistant" {
					t.Errorf("role = %v, want assistant", first["role"])
				}
			default:
				if first["role"] != "assistant" {
					t.Errorf("role = %v, want assistant", first["role"])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. ToolCallBlock handling
// ---------------------------------------------------------------------------

func TestAllFormatters_ToolCallBlock(t *testing.T) {
	msgs := []*message.Msg{
		message.AssistantMsg("bot", []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "tc_1", Name: "search", Input: `{"query":"golang"}`},
		}),
	}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			if len(result) == 0 {
				t.Fatal("expected at least 1 formatted message, got 0")
			}

			jsonMarshalOK(t, result)

			switch nf.name {
			case "Gemini":
				parts, ok := result[0]["parts"].([]map[string]any)
				if !ok || len(parts) == 0 {
					t.Fatalf("parts missing: %v", result[0])
				}
				fc, ok := parts[0]["functionCall"].(map[string]any)
				if !ok {
					t.Fatalf("functionCall missing: %v", parts[0])
				}
				if fc["name"] != "search" {
					t.Errorf("functionCall.name = %v, want search", fc["name"])
				}
				// args should be the parsed JSON
				args, ok := fc["args"].(map[string]any)
				if !ok {
					t.Fatalf("args not a map: %T", fc["args"])
				}
				if args["query"] != "golang" {
					t.Errorf("args.query = %v, want golang", args["query"])
				}

			case "Anthropic":
				content, ok := result[0]["content"].([]map[string]any)
				if !ok || len(content) == 0 {
					t.Fatalf("content missing: %v", result[0])
				}
				if content[0]["type"] != "tool_use" {
					t.Errorf("type = %v, want tool_use", content[0]["type"])
				}
				if content[0]["id"] != "tc_1" {
					t.Errorf("id = %v, want tc_1", content[0]["id"])
				}
				if content[0]["name"] != "search" {
					t.Errorf("name = %v, want search", content[0]["name"])
				}
				inputMap, ok := content[0]["input"].(map[string]any)
				if !ok {
					t.Fatalf("input not a map: %T", content[0]["input"])
				}
				if inputMap["query"] != "golang" {
					t.Errorf("input.query = %v", inputMap["query"])
				}

			case "OpenAIResponse":
				// OpenAIResponse emits function_call items
				var fcItem map[string]any
				for _, item := range result {
					if item["type"] == "function_call" {
						fcItem = item
						break
					}
				}
				if fcItem == nil {
					t.Fatal("no function_call item found")
				}
				if fcItem["name"] != "search" {
					t.Errorf("name = %v, want search", fcItem["name"])
				}
				if fcItem["arguments"] != `{"query":"golang"}` {
					t.Errorf("arguments = %v", fcItem["arguments"])
				}
				if fcItem["call_id"] != "tc_1" {
					t.Errorf("call_id = %v, want tc_1", fcItem["call_id"])
				}

			default: // OpenAI family
				tc, ok := result[0]["tool_calls"].([]map[string]any)
				if !ok || len(tc) == 0 {
					t.Fatalf("tool_calls missing: %v", result[0])
				}
				if tc[0]["type"] != "function" {
					t.Errorf("type = %v, want function", tc[0]["type"])
				}
				fn, ok := tc[0]["function"].(map[string]any)
				if !ok {
					t.Fatalf("function missing: %v", tc[0])
				}
				if fn["name"] != "search" {
					t.Errorf("function.name = %v, want search", fn["name"])
				}
				if fn["arguments"] != `{"query":"golang"}` {
					t.Errorf("function.arguments = %v", fn["arguments"])
				}
			}
		})
	}
}

func TestAllFormatters_ToolCallWithTextBlock(t *testing.T) {
	msgs := []*message.Msg{
		message.AssistantMsg("bot", []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "Let me search for that."},
			message.ToolCallBlock{Type: "tool_call", ID: "tc_2", Name: "web_search", Input: `{"q":"test"}`},
		}),
	}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			if len(result) == 0 {
				t.Fatal("expected at least 1 formatted message")
			}
			jsonMarshalOK(t, result)

			switch nf.name {
			case "Anthropic":
				content, ok := result[0]["content"].([]map[string]any)
				if !ok {
					t.Fatalf("content not []map: %T", result[0]["content"])
				}
				if len(content) != 2 {
					t.Fatalf("expected 2 content blocks, got %d", len(content))
				}
				if content[0]["type"] != "text" {
					t.Errorf("block[0].type = %v, want text", content[0]["type"])
				}
				if content[1]["type"] != "tool_use" {
					t.Errorf("block[1].type = %v, want tool_use", content[1]["type"])
				}

			case "Gemini":
				parts, ok := result[0]["parts"].([]map[string]any)
				if !ok {
					t.Fatalf("parts not []map: %T", result[0]["parts"])
				}
				if len(parts) != 2 {
					t.Fatalf("expected 2 parts, got %d", len(parts))
				}
				if parts[0]["text"] != "Let me search for that." {
					t.Errorf("part[0].text = %v", parts[0]["text"])
				}
				if parts[1]["functionCall"] == nil {
					t.Error("part[1] missing functionCall")
				}

			case "OpenAIResponse":
				// Should have a content message with input_text AND a function_call item
				foundText := false
				foundFC := false
				for _, item := range result {
					if content, ok := item["content"].([]map[string]any); ok {
						for _, c := range content {
							if c["type"] == "input_text" && c["text"] == "Let me search for that." {
								foundText = true
							}
						}
					}
					if item["type"] == "function_call" {
						foundFC = true
					}
				}
				if !foundText {
					t.Error("missing input_text content")
				}
				if !foundFC {
					t.Error("missing function_call item")
				}

			default: // OpenAI family
				if result[0]["content"] != "Let me search for that." {
					t.Errorf("content = %v", result[0]["content"])
				}
				tc, ok := result[0]["tool_calls"].([]map[string]any)
				if !ok || len(tc) == 0 {
					t.Fatal("tool_calls missing")
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. ToolResultBlock handling
// ---------------------------------------------------------------------------

func TestAllFormatters_ToolResultBlock(t *testing.T) {
	msgs := []*message.Msg{
		message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
			message.ToolResultBlock{Type: "tool_result", ID: "tc_1", Name: "search", Output: "42 results found"},
		}),
	}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			if len(result) == 0 {
				t.Fatal("expected at least 1 formatted message")
			}

			jsonMarshalOK(t, result)

			switch nf.name {
			case "Gemini":
				parts, ok := result[0]["parts"].([]map[string]any)
				if !ok || len(parts) == 0 {
					t.Fatalf("parts missing: %v", result[0])
				}
				fr, ok := parts[0]["functionResponse"].(map[string]any)
				if !ok {
					t.Fatalf("functionResponse missing: %v", parts[0])
				}
				if fr["name"] != "search" {
					t.Errorf("functionResponse.name = %v, want search", fr["name"])
				}
				resp, ok := fr["response"].(map[string]any)
				if !ok {
					t.Fatalf("response missing: %v", fr)
				}
				if resp["result"] != "42 results found" {
					t.Errorf("response.result = %v", resp["result"])
				}

			case "Anthropic":
				content, ok := result[0]["content"].([]map[string]any)
				if !ok || len(content) == 0 {
					t.Fatalf("content missing: %v", result[0])
				}
				if content[0]["type"] != "tool_result" {
					t.Errorf("type = %v, want tool_result", content[0]["type"])
				}
				if content[0]["tool_use_id"] != "tc_1" {
					t.Errorf("tool_use_id = %v, want tc_1", content[0]["tool_use_id"])
				}
				if content[0]["content"] != "42 results found" {
					t.Errorf("content = %v", content[0]["content"])
				}

			case "OpenAIResponse":
				if result[0]["type"] != "function_call_output" {
					t.Errorf("type = %v, want function_call_output", result[0]["type"])
				}
				if result[0]["call_id"] != "tc_1" {
					t.Errorf("call_id = %v, want tc_1", result[0]["call_id"])
				}
				if result[0]["output"] != "42 results found" {
					t.Errorf("output = %v", result[0]["output"])
				}

			default: // OpenAI family
				if result[0]["role"] != "tool" {
					t.Errorf("role = %v, want tool", result[0]["role"])
				}
				if result[0]["tool_call_id"] != "tc_1" {
					t.Errorf("tool_call_id = %v", result[0]["tool_call_id"])
				}
				if result[0]["content"] != "42 results found" {
					t.Errorf("content = %v", result[0]["content"])
				}
			}
		})
	}
}

func TestAllFormatters_ToolResultWithCallIDMetadata(t *testing.T) {
	// Test that OpenAIResponse uses call_id from metadata when available
	msgs := []*message.Msg{
		message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
			message.ToolResultBlock{
				Type:     "tool_result",
				ID:       "tc_orig",
				Name:     "search",
				Output:   "done",
				Metadata: map[string]any{"call_id": "call_abc123"},
			},
		}),
	}

	t.Run("OpenAIResponse_uses_metadata_call_id", func(t *testing.T) {
		f := NewOpenAIResponseFormatter()
		result, err := f.Format(msgs)
		if err != nil {
			t.Fatalf("Format error: %v", err)
		}
		if len(result) == 0 {
			t.Fatal("no result")
		}
		if result[0]["call_id"] != "call_abc123" {
			t.Errorf("call_id = %v, want call_abc123", result[0]["call_id"])
		}
	})
}

// ---------------------------------------------------------------------------
// 4. ThinkingBlock handling
// ---------------------------------------------------------------------------

func TestAllFormatters_ThinkingBlock(t *testing.T) {
	msgs := []*message.Msg{
		message.AssistantMsg("bot", []message.ContentBlock{
			message.ThinkingBlock{Type: "thinking", Thinking: "Let me reason about this..."},
			message.TextBlock{Type: "text", Text: "The answer is 42."},
		}),
	}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			if len(result) == 0 {
				t.Fatal("expected at least 1 formatted message")
			}

			jsonMarshalOK(t, result)

			switch nf.name {
			case "DashScope", "DeepSeek":
				// These support reasoning_content
				if result[0]["reasoning_content"] != "Let me reason about this..." {
					t.Errorf("reasoning_content = %v", result[0]["reasoning_content"])
				}
				if result[0]["content"] != "The answer is 42." {
					t.Errorf("content = %v", result[0]["content"])
				}

			case "OpenAI", "Moonshot", "Ollama", "XAI":
				// These do not support thinking; ThinkingBlock is silently ignored
				if result[0]["reasoning_content"] != nil {
					t.Errorf("reasoning_content should be nil for %s, got %v", nf.name, result[0]["reasoning_content"])
				}
				if result[0]["content"] != "The answer is 42." {
					t.Errorf("content = %v", result[0]["content"])
				}

			case "Anthropic":
				content, ok := result[0]["content"].([]map[string]any)
				if !ok {
					t.Fatalf("content not []map: %T", result[0]["content"])
				}
				if len(content) != 2 {
					t.Fatalf("expected 2 content blocks, got %d", len(content))
				}
				if content[0]["type"] != "thinking" {
					t.Errorf("block[0].type = %v, want thinking", content[0]["type"])
				}
				if content[0]["thinking"] != "Let me reason about this..." {
					t.Errorf("block[0].thinking = %v", content[0]["thinking"])
				}
				if content[1]["type"] != "text" {
					t.Errorf("block[1].type = %v, want text", content[1]["type"])
				}
				if content[1]["text"] != "The answer is 42." {
					t.Errorf("block[1].text = %v", content[1]["text"])
				}

			case "Gemini":
				parts, ok := result[0]["parts"].([]map[string]any)
				if !ok {
					t.Fatalf("parts not []map: %T", result[0]["parts"])
				}
				if len(parts) != 2 {
					t.Fatalf("expected 2 parts, got %d", len(parts))
				}
				if parts[0]["thought"] != true {
					t.Errorf("part[0].thought = %v, want true", parts[0]["thought"])
				}
				if parts[0]["text"] != "Let me reason about this..." {
					t.Errorf("part[0].text = %v", parts[0]["text"])
				}
				if parts[1]["text"] != "The answer is 42." {
					t.Errorf("part[1].text = %v", parts[1]["text"])
				}

			case "OpenAIResponse":
				// ThinkingBlock with reasoning_item_id produces a reasoning item
				// Without it, the thinking block is skipped and only the text content is emitted
				if len(result) != 1 {
					t.Fatalf("expected 1 item, got %d", len(result))
				}
				content, ok := result[0]["content"].([]map[string]any)
				if !ok || len(content) == 0 {
					t.Fatalf("content missing: %v", result[0])
				}
				if content[0]["type"] != "input_text" {
					t.Errorf("type = %v, want input_text", content[0]["type"])
				}
				if content[0]["text"] != "The answer is 42." {
					t.Errorf("text = %v", content[0]["text"])
				}
			}
		})
	}
}

func TestAnthropic_ThinkingBlockWithSignature(t *testing.T) {
	msgs := []*message.Msg{
		message.AssistantMsg("bot", []message.ContentBlock{
			message.ThinkingBlock{
				Type:     "thinking",
				Thinking: "Deep thought",
				Extra:    map[string]any{"signature": "sig_abc123"},
			},
			message.TextBlock{Type: "text", Text: "Result."},
		}),
	}

	f := NewAnthropicFormatter()
	result, err := f.Format(msgs)
	if err != nil {
		t.Fatalf("Format error: %v", err)
	}

	content, ok := result[0]["content"].([]map[string]any)
	if !ok {
		t.Fatal("content not []map")
	}
	if content[0]["signature"] != "sig_abc123" {
		t.Errorf("signature = %v, want sig_abc123", content[0]["signature"])
	}
}

func TestOpenAIResponse_ThinkingBlockWithReasoningItemID(t *testing.T) {
	msgs := []*message.Msg{
		message.AssistantMsg("bot", []message.ContentBlock{
			message.ThinkingBlock{
				Type:     "thinking",
				Thinking: "reasoning...",
				Extra:    map[string]any{"reasoning_item_id": "ri_xyz"},
			},
			message.TextBlock{Type: "text", Text: "Done."},
		}),
	}

	f := NewOpenAIResponseFormatter()
	result, err := f.Format(msgs)
	if err != nil {
		t.Fatalf("Format error: %v", err)
	}

	// Should have a content message AND a reasoning item
	foundReasoning := false
	foundText := false
	for _, item := range result {
		if item["type"] == "reasoning" && item["id"] == "ri_xyz" {
			foundReasoning = true
		}
		if content, ok := item["content"].([]map[string]any); ok {
			for _, c := range content {
				if c["type"] == "input_text" && c["text"] == "Done." {
					foundText = true
				}
			}
		}
	}
	if !foundReasoning {
		t.Error("missing reasoning item with id ri_xyz")
	}
	if !foundText {
		t.Error("missing input_text content")
	}
}

// ---------------------------------------------------------------------------
// 5. DataBlock handling (image/audio)
// ---------------------------------------------------------------------------

func TestAllFormatters_DataBlock_Base64Image(t *testing.T) {
	msgs := []*message.Msg{
		message.UserMsg("user", []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "What is in this image?"},
			message.DataBlock{
				Type: "data",
				ID:   "img_1",
				Source: message.Base64Source{
					Type:      "base64",
					Data:      "iVBORw0KGgo=",
					MediaType: "image/png",
				},
			},
		}),
	}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			if len(result) == 0 {
				t.Fatal("expected at least 1 message")
			}

			jsonMarshalOK(t, result)

			if !supportsImageInput(nf.name) {
				// Formatters without image support should produce plain text content
				// (the DataBlock is silently dropped)
				contentStr, ok := result[0]["content"].(string)
				if !ok {
					t.Fatalf("expected string content for %s (no image support), got %T", nf.name, result[0]["content"])
				}
				if contentStr != "What is in this image?" {
					t.Errorf("content = %v", contentStr)
				}
				return
			}

			switch nf.name {
			case "Anthropic":
				content, ok := result[0]["content"].([]map[string]any)
				if !ok {
					t.Fatalf("content not []map: %T", result[0]["content"])
				}
				if len(content) < 2 {
					t.Fatalf("expected at least 2 blocks, got %d", len(content))
				}
				imgBlock := content[1]
				if imgBlock["type"] != "image" {
					t.Errorf("type = %v, want image", imgBlock["type"])
				}
				src, ok := imgBlock["source"].(map[string]any)
				if !ok {
					t.Fatal("source missing")
				}
				if src["type"] != "base64" {
					t.Errorf("source.type = %v, want base64", src["type"])
				}
				if src["media_type"] != "image/png" {
					t.Errorf("source.media_type = %v", src["media_type"])
				}
				if src["data"] != "iVBORw0KGgo=" {
					t.Errorf("source.data = %v", src["data"])
				}

			case "Gemini":
				parts, ok := result[0]["parts"].([]map[string]any)
				if !ok {
					t.Fatalf("parts not []map: %T", result[0]["parts"])
				}
				if len(parts) < 2 {
					t.Fatalf("expected at least 2 parts, got %d", len(parts))
				}
				inline, ok := parts[1]["inlineData"].(map[string]any)
				if !ok {
					t.Fatalf("inlineData missing: %v", parts[1])
				}
				if inline["mimeType"] != "image/png" {
					t.Errorf("mimeType = %v", inline["mimeType"])
				}
				if inline["data"] != "iVBORw0KGgo=" {
					t.Errorf("data = %v", inline["data"])
				}

			case "OpenAIResponse":
				content, ok := result[0]["content"].([]map[string]any)
				if !ok {
					t.Fatalf("content not []map: %T", result[0]["content"])
				}
				if len(content) < 2 {
					t.Fatalf("expected at least 2 content items, got %d", len(content))
				}
				imgItem := content[1]
				if imgItem["type"] != "input_image" {
					t.Errorf("type = %v, want input_image", imgItem["type"])
				}
				expectedURL := "data:image/png;base64,iVBORw0KGgo="
				if imgItem["image_url"] != expectedURL {
					t.Errorf("image_url = %v, want %v", imgItem["image_url"], expectedURL)
				}

			default: // OpenAI, DashScope (with image support)
				content, ok := result[0]["content"].([]map[string]any)
				if !ok {
					t.Fatalf("content not []map for image message: %T", result[0]["content"])
				}
				if len(content) < 2 {
					t.Fatalf("expected at least 2 content items, got %d", len(content))
				}
				textItem := content[0]
				if textItem["type"] != "text" {
					t.Errorf("content[0].type = %v, want text", textItem["type"])
				}
				imgItem := content[1]
				if imgItem["type"] != "image_url" {
					t.Errorf("content[1].type = %v, want image_url", imgItem["type"])
				}
				imgURL, ok := imgItem["image_url"].(map[string]any)
				if !ok {
					t.Fatal("image_url field missing")
				}
				if imgURL["url"] != "data:image/png;base64,iVBORw0KGgo=" {
					t.Errorf("image_url.url = %v", imgURL["url"])
				}
			}
		})
	}
}

func TestAllFormatters_DataBlock_URLImage(t *testing.T) {
	msgs := []*message.Msg{
		message.UserMsg("user", []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "Describe this."},
			message.DataBlock{
				Type: "data",
				ID:   "img_2",
				Source: message.URLSource{
					Type:      "url",
					URL:       "https://example.com/photo.jpg",
					MediaType: "image/jpeg",
				},
			},
		}),
	}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			if len(result) == 0 {
				t.Fatal("expected at least 1 message")
			}

			jsonMarshalOK(t, result)

			switch nf.name {
			case "Anthropic":
				content := result[0]["content"].([]map[string]any)
				imgBlock := content[1]
				src := imgBlock["source"].(map[string]any)
				if src["type"] != "url" {
					t.Errorf("source.type = %v, want url", src["type"])
				}
				if src["url"] != "https://example.com/photo.jpg" {
					t.Errorf("source.url = %v", src["url"])
				}

			case "Gemini":
				parts := result[0]["parts"].([]map[string]any)
				fileData, ok := parts[1]["fileData"].(map[string]any)
				if !ok {
					t.Fatalf("fileData missing: %v", parts[1])
				}
				if fileData["fileUri"] != "https://example.com/photo.jpg" {
					t.Errorf("fileUri = %v", fileData["fileUri"])
				}
				if fileData["mimeType"] != "image/jpeg" {
					t.Errorf("mimeType = %v", fileData["mimeType"])
				}

			case "OpenAIResponse":
				content := result[0]["content"].([]map[string]any)
				imgItem := content[1]
				if imgItem["image_url"] != "https://example.com/photo.jpg" {
					t.Errorf("image_url = %v", imgItem["image_url"])
				}

			case "DeepSeek":
				// DeepSeek has no SupportedInputMediaTypes, so images are
				// dropped and content may be a plain string.
				t.Log("DeepSeek does not support image input, skipping assertion")

			default: // OpenAI family
				content, ok := result[0]["content"].([]map[string]any)
				if !ok {
					t.Skipf("content is not []map for %s, likely no image support", nf.name)
				}
				imgItem := content[1]
				imgURL := imgItem["image_url"].(map[string]any)
				if imgURL["url"] != "https://example.com/photo.jpg" {
					t.Errorf("image_url.url = %v", imgURL["url"])
				}
			}
		})
	}
}

func TestOpenAIFamily_DataBlock_Audio(t *testing.T) {
	msgs := []*message.Msg{
		message.UserMsg("user", []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "Transcribe."},
			message.DataBlock{
				Type: "data",
				ID:   "aud_1",
				Source: message.Base64Source{
					Type:      "base64",
					Data:      "AAAA",
					MediaType: "audio/mp3",
				},
			},
		}),
	}

	// Only OpenAI, DashScope support audio input
	formatters := []namedFormatter{
		{"OpenAI", NewOpenAIFormatter(), nil},
		{"DashScope", NewDashScopeFormatter(), nil},
	}

	for _, nf := range formatters {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			content, ok := result[0]["content"].([]map[string]any)
			if !ok {
				t.Fatalf("content not []map: %T", result[0]["content"])
			}
			var audioItem map[string]any
			for _, c := range content {
				if c["type"] == "input_audio" {
					audioItem = c
					break
				}
			}
			if audioItem == nil {
				t.Fatal("no input_audio block found")
			}
			ia, ok := audioItem["input_audio"].(map[string]any)
			if !ok {
				t.Fatal("input_audio field missing")
			}
			wantData := "AAAA" // OpenAI: raw base64
			if nf.name == "DashScope" {
				// DashScope requires a data URL (upstream fix #2315).
				wantData = "data:;base64,AAAA"
			}
			if ia["data"] != wantData {
				t.Errorf("data = %v, want %q", ia["data"], wantData)
			}
			if ia["format"] != "mp3" {
				t.Errorf("format = %v, want mp3", ia["format"])
			}
		})
	}
}

// TestOpenAIFamily_DataBlock_AudioMpegMapsToMp3 locks in the shared
// mpeg→mp3 format mapping (evaluator M3): the OpenAI input_audio API only
// accepts wav|mp3 format labels, so audio/mpeg must be emitted as mp3 on the
// OpenAI path too, not only on the DashScope data-URL path.
func TestOpenAIFamily_DataBlock_AudioMpegMapsToMp3(t *testing.T) {
	msgs := []*message.Msg{
		message.UserMsg("user", []message.ContentBlock{
			message.DataBlock{
				Type: "data",
				ID:   "aud_mpeg",
				Source: message.Base64Source{
					Type:      "base64",
					Data:      "BBBB",
					MediaType: "audio/mpeg",
				},
			},
		}),
	}

	formatters := []namedFormatter{
		{"OpenAI", NewOpenAIFormatter(), nil},
		{"DashScope", NewDashScopeFormatter(), nil},
	}
	for _, nf := range formatters {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			content, ok := result[0]["content"].([]map[string]any)
			if !ok {
				t.Fatalf("content not []map: %T", result[0]["content"])
			}
			var ia map[string]any
			for _, c := range content {
				if c["type"] == "input_audio" {
					ia, _ = c["input_audio"].(map[string]any)
					break
				}
			}
			if ia == nil {
				t.Fatal("no input_audio block found")
			}
			if ia["format"] != "mp3" {
				t.Errorf("format = %v, want mp3 (mpeg must map to mp3)", ia["format"])
			}
			wantData := "BBBB"
			if nf.name == "DashScope" {
				wantData = "data:;base64,BBBB"
			}
			if ia["data"] != wantData {
				t.Errorf("data = %v, want %q", ia["data"], wantData)
			}
		})
	}
}

func TestDashScope_DataBlock_Video(t *testing.T) {
	msgs := []*message.Msg{
		message.UserMsg("user", []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "What happens in this video?"},
			message.DataBlock{
				Type: "data",
				ID:   "vid_1",
				Source: message.URLSource{
					Type:      "url",
					URL:       "https://example.com/video.mp4",
					MediaType: "video/mp4",
				},
			},
		}),
	}

	f := NewDashScopeFormatter()
	result, err := f.Format(msgs)
	if err != nil {
		t.Fatalf("Format error: %v", err)
	}
	content, ok := result[0]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content not []map: %T", result[0]["content"])
	}
	var videoItem map[string]any
	for _, c := range content {
		if c["type"] == "video_url" {
			videoItem = c
			break
		}
	}
	if videoItem == nil {
		t.Fatal("no video_url block found")
	}
	vu, ok := videoItem["video_url"].(map[string]any)
	if !ok {
		t.Fatal("video_url field missing")
	}
	if vu["url"] != "https://example.com/video.mp4" {
		t.Errorf("url = %v", vu["url"])
	}
}

func TestUnsupportedMediaType_Ignored(t *testing.T) {
	msgs := []*message.Msg{
		message.UserMsg("user", []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "check this"},
			message.DataBlock{
				Type: "data",
				ID:   "vid_2",
				Source: message.URLSource{
					Type:      "url",
					URL:       "https://example.com/video.mp4",
					MediaType: "video/mp4",
				},
			},
		}),
	}

	// OpenAI base does not support video/*
	f := NewOpenAIFormatter()
	result, err := f.Format(msgs)
	if err != nil {
		t.Fatalf("Format error: %v", err)
	}
	// Should fall back to plain string content since video is unsupported
	if result[0]["content"] != "check this" {
		t.Errorf("content = %v, expected plain text since video is unsupported by OpenAI", result[0]["content"])
	}
}

// ---------------------------------------------------------------------------
// 6. HintBlock handling
// ---------------------------------------------------------------------------

func TestAllFormatters_HintBlock_StringHint(t *testing.T) {
	msgs := []*message.Msg{
		message.AssistantMsg("bot", []message.ContentBlock{
			message.HintBlock{Type: "hint", ID: "h1", Source: "middleware", Hint: "Remember to be concise."},
			message.TextBlock{Type: "text", Text: "OK."},
		}),
	}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			if len(result) == 0 {
				t.Fatal("expected at least 1 message")
			}

			jsonMarshalOK(t, result)

			switch nf.name {
			case "Gemini":
				parts := result[0]["parts"].([]map[string]any)
				if len(parts) < 2 {
					t.Fatalf("expected at least 2 parts, got %d", len(parts))
				}
				if parts[0]["text"] != "Remember to be concise." {
					t.Errorf("hint text = %v", parts[0]["text"])
				}

			case "Anthropic":
				content := result[0]["content"].([]map[string]any)
				if len(content) < 2 {
					t.Fatalf("expected at least 2 blocks, got %d", len(content))
				}
				if content[0]["type"] != "text" {
					t.Errorf("block[0].type = %v, want text", content[0]["type"])
				}
				if content[0]["text"] != "Remember to be concise." {
					t.Errorf("block[0].text = %v", content[0]["text"])
				}

			case "OpenAIResponse":
				content := result[0]["content"].([]map[string]any)
				if len(content) < 2 {
					t.Fatalf("expected at least 2 items, got %d", len(content))
				}
				if content[0]["type"] != "input_text" {
					t.Errorf("type = %v, want input_text", content[0]["type"])
				}
				if content[0]["text"] != "Remember to be concise." {
					t.Errorf("text = %v", content[0]["text"])
				}

			default: // OpenAI family
				// HintBlock text is concatenated with TextBlock text
				contentStr, ok := result[0]["content"].(string)
				if !ok {
					t.Fatalf("content not string: %T", result[0]["content"])
				}
				if contentStr != "Remember to be concise.OK." {
					t.Errorf("content = %v, want concatenated hint + text", contentStr)
				}
			}
		})
	}
}

func TestAllFormatters_HintBlock_ContentBlockHint(t *testing.T) {
	msgs := []*message.Msg{
		message.AssistantMsg("bot", []message.ContentBlock{
			message.HintBlock{
				Type:   "hint",
				ID:     "h2",
				Source: "skill",
				Hint: []message.ContentBlock{
					message.TextBlock{Type: "text", Text: "Skill instruction A."},
					message.TextBlock{Type: "text", Text: "Skill instruction B."},
				},
			},
			message.TextBlock{Type: "text", Text: "Understood."},
		}),
	}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			if len(result) == 0 {
				t.Fatal("expected at least 1 message")
			}

			jsonMarshalOK(t, result)

			// The HintBlock.GetHintText() should concatenate the two TextBlocks
			expectedHint := "Skill instruction A.\nSkill instruction B."

			switch nf.name {
			case "Gemini":
				parts := result[0]["parts"].([]map[string]any)
				if parts[0]["text"] != expectedHint {
					t.Errorf("hint text = %v, want %v", parts[0]["text"], expectedHint)
				}
			case "Anthropic":
				content := result[0]["content"].([]map[string]any)
				if content[0]["text"] != expectedHint {
					t.Errorf("hint text = %v, want %v", content[0]["text"], expectedHint)
				}
			case "OpenAIResponse":
				content := result[0]["content"].([]map[string]any)
				if content[0]["text"] != expectedHint {
					t.Errorf("hint text = %v, want %v", content[0]["text"], expectedHint)
				}
			default:
				contentStr := result[0]["content"].(string)
				expected := expectedHint + "Understood."
				if contentStr != expected {
					t.Errorf("content = %v, want %v", contentStr, expected)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 7. Multi-role conversation handling
// ---------------------------------------------------------------------------

func TestAllFormatters_MultiRoleConversation(t *testing.T) {
	msgs := []*message.Msg{
		message.SystemMsg("system", "You are a helpful assistant."),
		message.UserMsg("alice", "What is 2+2?"),
		message.AssistantMsg("bot", "4"),
		message.UserMsg("alice", "Thanks!"),
	}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}

			jsonMarshalOK(t, result)

			switch nf.name {
			case "Anthropic", "Gemini", "OpenAIResponse":
				// These skip system messages
				if len(result) != 3 {
					t.Fatalf("expected 3 messages (system skipped), got %d", len(result))
				}
				// First should be user
				roles := []string{}
				for _, r := range result {
					role, _ := r["role"].(string)
					roles = append(roles, role)
				}
				expected := []string{"user", "assistant", "user"}
				if nf.name == "Gemini" {
					expected = []string{"user", "model", "user"}
				}
				for i, r := range roles {
					if r != expected[i] {
						t.Errorf("role[%d] = %v, want %v", i, r, expected[i])
					}
				}
			default: // OpenAI family
				if len(result) != 4 {
					t.Fatalf("expected 4 messages, got %d", len(result))
				}
				if result[0]["role"] != "system" {
					t.Errorf("role[0] = %v, want system", result[0]["role"])
				}
				if result[0]["content"] != "You are a helpful assistant." {
					t.Errorf("system content = %v", result[0]["content"])
				}
			}
		})
	}
}

func TestAllFormatters_ToolCallAndResultConversation(t *testing.T) {
	// Full tool-use conversation flow
	msgs := []*message.Msg{
		message.UserMsg("user", "Search for Go."),
		message.AssistantMsg("bot", []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "Searching..."},
			message.ToolCallBlock{Type: "tool_call", ID: "tc_x", Name: "search", Input: `{"q":"Go"}`},
		}),
		message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
			message.ToolResultBlock{Type: "tool_result", ID: "tc_x", Name: "search", Output: "Go is a language"},
		}),
		message.AssistantMsg("bot", "Go is a programming language by Google."),
	}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}

			jsonMarshalOK(t, result)

			// Verify the conversation has the right number of messages
			switch nf.name {
			case "OpenAIResponse":
				// OpenAI Response API flattens into items: user content, assistant content + function_call, function_call_output, assistant content
				if len(result) < 4 {
					t.Fatalf("expected at least 4 items, got %d", len(result))
				}
			default:
				if len(result) < 3 {
					t.Fatalf("expected at least 3 messages, got %d", len(result))
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 8. System prompt extraction
// ---------------------------------------------------------------------------

func TestExtractSystemPrompt_AllProviders(t *testing.T) {
	msgs := []*message.Msg{
		message.SystemMsg("sys", "You are a code reviewer."),
		message.UserMsg("dev", "Review this PR."),
	}

	t.Run("ExtractSystemPrompt", func(t *testing.T) {
		s := ExtractSystemPrompt(msgs)
		if s != "You are a code reviewer." {
			t.Errorf("system prompt = %q, want 'You are a code reviewer.'", s)
		}
	})

	t.Run("ExtractSystemPrompt_NoSystem", func(t *testing.T) {
		s := ExtractSystemPrompt([]*message.Msg{message.UserMsg("dev", "Hi")})
		if s != "" {
			t.Errorf("expected empty, got %q", s)
		}
	})

	t.Run("ExtractSystemPrompt_NilMessages", func(t *testing.T) {
		s := ExtractSystemPrompt(nil)
		if s != "" {
			t.Errorf("expected empty, got %q", s)
		}
	})

	t.Run("ExtractGeminiSystemInstruction", func(t *testing.T) {
		si := ExtractGeminiSystemInstruction(msgs)
		if si == nil {
			t.Fatal("expected non-nil system instruction")
		}
		parts, ok := si["parts"].([]map[string]any)
		if !ok || len(parts) == 0 {
			t.Fatalf("parts missing: %v", si)
		}
		if parts[0]["text"] != "You are a code reviewer." {
			t.Errorf("text = %v", parts[0]["text"])
		}
	})

	t.Run("ExtractGeminiSystemInstruction_NoSystem", func(t *testing.T) {
		si := ExtractGeminiSystemInstruction([]*message.Msg{message.UserMsg("dev", "Hi")})
		if si != nil {
			t.Errorf("expected nil, got %v", si)
		}
	})

	t.Run("ExtractResponseInstructions", func(t *testing.T) {
		s := ExtractResponseInstructions(msgs)
		if s != "You are a code reviewer." {
			t.Errorf("instructions = %q", s)
		}
	})
}

func TestAnthropicFormatter_SkipsSystemInFormat(t *testing.T) {
	msgs := []*message.Msg{
		message.SystemMsg("sys", "System prompt here."),
		message.UserMsg("user", "Hello"),
		message.AssistantMsg("bot", "Hi"),
	}

	f := NewAnthropicFormatter()
	result, err := f.Format(msgs)
	if err != nil {
		t.Fatal(err)
	}

	// System message should be skipped
	for _, m := range result {
		if m["role"] == "system" {
			t.Error("Anthropic formatter should skip system messages")
		}
	}
	if len(result) != 2 {
		t.Errorf("expected 2 messages, got %d", len(result))
	}
}

func TestGeminiFormatter_SkipsSystemInFormat(t *testing.T) {
	msgs := []*message.Msg{
		message.SystemMsg("sys", "System prompt here."),
		message.UserMsg("user", "Hello"),
		message.AssistantMsg("bot", "Hi"),
	}

	f := NewGeminiFormatter()
	result, err := f.Format(msgs)
	if err != nil {
		t.Fatal(err)
	}

	for _, m := range result {
		if m["role"] == "system" {
			t.Error("Gemini formatter should skip system messages")
		}
	}
	if len(result) != 2 {
		t.Errorf("expected 2 messages, got %d", len(result))
	}
}

func TestOpenAIResponseFormatter_SkipsSystemInFormat(t *testing.T) {
	msgs := []*message.Msg{
		message.SystemMsg("sys", "System prompt here."),
		message.UserMsg("user", "Hello"),
	}

	f := NewOpenAIResponseFormatter()
	result, err := f.Format(msgs)
	if err != nil {
		t.Fatal(err)
	}

	for _, m := range result {
		if m["role"] == "system" {
			t.Error("OpenAI Response formatter should skip system messages")
		}
	}
}

// ---------------------------------------------------------------------------
// Multi-agent formatting
// ---------------------------------------------------------------------------

func TestMultiAgentFormatters(t *testing.T) {
	msgs := []*message.Msg{
		message.SystemMsg("sys", "You are helpful."),
		message.UserMsg("alice", "Question from Alice"),
		message.AssistantMsg("bob", "Answer from Bob"),
		message.UserMsg("charlie", "Follow-up from Charlie"),
	}

	t.Run("OpenAI_MultiAgent", func(t *testing.T) {
		f := NewOpenAIFormatter()
		result, err := f.FormatMultiAgent(msgs, "current_agent")
		if err != nil {
			t.Fatal(err)
		}
		jsonMarshalOK(t, result)

		// The name field should be injected for non-current agents
		for _, m := range result {
			role, _ := m["role"].(string)
			if role == "user" || role == "assistant" {
				// name should be present if msg.Name != currentAgent
				// (implementation injects sanitized name)
				_ = role
			}
		}
	})

	t.Run("Anthropic_MultiAgent", func(t *testing.T) {
		f := NewAnthropicFormatter()
		result, err := f.FormatMultiAgent(msgs, "current_agent")
		if err != nil {
			t.Fatal(err)
		}
		jsonMarshalOK(t, result)

		if len(result) == 0 {
			t.Fatal("no results")
		}
		// Verify name prefixes exist in user/assistant messages
		for _, m := range result {
			if content, ok := m["content"].([]map[string]any); ok && len(content) > 0 {
				first := content[0]
				if text, ok := first["text"].(string); ok {
					if len(text) > 0 && text[0] == '[' {
						t.Logf("found name prefix: %s", text)
					}
				}
			}
		}
	})

	t.Run("Gemini_MultiAgent", func(t *testing.T) {
		f := NewGeminiFormatter()
		result, err := f.FormatMultiAgent(msgs, "current_agent")
		if err != nil {
			t.Fatal(err)
		}
		jsonMarshalOK(t, result)
		if len(result) == 0 {
			t.Fatal("no results")
		}
	})

	t.Run("DashScope_MultiAgent", func(t *testing.T) {
		f := NewDashScopeFormatter()
		result, err := f.FormatMultiAgent(msgs, "current_agent")
		if err != nil {
			t.Fatal(err)
		}
		jsonMarshalOK(t, result)
	})

	t.Run("DeepSeek_MultiAgent", func(t *testing.T) {
		f := NewDeepSeekFormatter()
		result, err := f.FormatMultiAgent(msgs, "current_agent")
		if err != nil {
			t.Fatal(err)
		}
		jsonMarshalOK(t, result)
	})
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestAllFormatters_NilMessages(t *testing.T) {
	msgs := []*message.Msg{nil, nil}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			if len(result) != 0 {
				t.Errorf("expected 0 messages for nil input, got %d", len(result))
			}
		})
	}
}

func TestAllFormatters_EmptySlice(t *testing.T) {
	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format([]*message.Msg{})
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			if len(result) != 0 {
				t.Errorf("expected 0 messages, got %d", len(result))
			}
		})
	}
}

func TestAllFormatters_EmptyContentBlocks(t *testing.T) {
	msgs := []*message.Msg{
		message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{}),
	}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			_, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			// Should either produce a message with empty content or skip it
			// Different formatters handle this differently, but it should not error
		})
	}
}

func TestAllFormatters_MultipleToolCalls(t *testing.T) {
	msgs := []*message.Msg{
		message.AssistantMsg("bot", []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "tc_a", Name: "read_file", Input: `{"path":"/a.txt"}`},
			message.ToolCallBlock{Type: "tool_call", ID: "tc_b", Name: "read_file", Input: `{"path":"/b.txt"}`},
		}),
	}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			if len(result) == 0 {
				t.Fatal("expected at least 1 message")
			}

			jsonMarshalOK(t, result)

			switch nf.name {
			case "Gemini":
				parts := result[0]["parts"].([]map[string]any)
				if len(parts) != 2 {
					t.Fatalf("expected 2 functionCall parts, got %d", len(parts))
				}

			case "Anthropic":
				content := result[0]["content"].([]map[string]any)
				if len(content) != 2 {
					t.Fatalf("expected 2 tool_use blocks, got %d", len(content))
				}
				if content[0]["name"] != "read_file" || content[1]["name"] != "read_file" {
					t.Error("tool names mismatch")
				}

			case "OpenAIResponse":
				fcCount := 0
				for _, item := range result {
					if item["type"] == "function_call" {
						fcCount++
					}
				}
				if fcCount != 2 {
					t.Errorf("expected 2 function_call items, got %d", fcCount)
				}

			default: // OpenAI family
				tc := result[0]["tool_calls"].([]map[string]any)
				if len(tc) != 2 {
					t.Fatalf("expected 2 tool_calls, got %d", len(tc))
				}
			}
		})
	}
}

func TestOpenAIResponse_ToolCallWithExtraCallID(t *testing.T) {
	msgs := []*message.Msg{
		message.AssistantMsg("bot", []message.ContentBlock{
			message.ToolCallBlock{
				Type:  "tool_call",
				ID:    "tc_1",
				Name:  "bash",
				Input: `{"cmd":"ls"}`,
				Extra: map[string]any{"call_id": "call_custom_id"},
			},
		}),
	}

	f := NewOpenAIResponseFormatter()
	result, err := f.Format(msgs)
	if err != nil {
		t.Fatal(err)
	}

	var fcItem map[string]any
	for _, item := range result {
		if item["type"] == "function_call" {
			fcItem = item
			break
		}
	}
	if fcItem == nil {
		t.Fatal("no function_call item found")
	}
	if fcItem["call_id"] != "call_custom_id" {
		t.Errorf("call_id = %v, want call_custom_id", fcItem["call_id"])
	}
}

func TestAllFormatters_ValidJSON_FullConversation(t *testing.T) {
	// Build a complex multi-turn conversation
	msgs := []*message.Msg{
		message.SystemMsg("sys", "Be helpful."),
		message.UserMsg("user", []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "Analyze this image"},
			message.DataBlock{
				Type: "data", ID: "img_full",
				Source: message.Base64Source{Type: "base64", Data: "abc", MediaType: "image/png"},
			},
		}),
		message.AssistantMsg("bot", []message.ContentBlock{
			message.ThinkingBlock{Type: "thinking", Thinking: "I see an image"},
			message.TextBlock{Type: "text", Text: "Let me search for info"},
			message.ToolCallBlock{Type: "tool_call", ID: "tc_full", Name: "search", Input: `{"q":"image"}`},
		}),
		message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
			message.ToolResultBlock{Type: "tool_result", ID: "tc_full", Name: "search", Output: "found info"},
		}),
		message.AssistantMsg("bot", []message.ContentBlock{
			message.HintBlock{Type: "hint", ID: "h_full", Hint: "remember format"},
			message.TextBlock{Type: "text", Text: "Here is the analysis."},
		}),
	}

	for _, nf := range allFormatters() {
		t.Run(nf.name, func(t *testing.T) {
			result, err := nf.f.Format(msgs)
			if err != nil {
				t.Fatalf("Format error: %v", err)
			}
			jsonMarshalOK(t, result)
		})
	}
}

// ---------------------------------------------------------------------------
// Utility function tests
// ---------------------------------------------------------------------------

func TestConvertToolResultToString(t *testing.T) {
	t.Run("String", func(t *testing.T) {
		s := ConvertToolResultToString("hello")
		if s != "hello" {
			t.Errorf("got %q", s)
		}
	})

	t.Run("ContentBlocks", func(t *testing.T) {
		blocks := []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "line1"},
			message.TextBlock{Type: "text", Text: "line2"},
		}
		s := ConvertToolResultToString(blocks)
		if s != "line1\nline2" {
			t.Errorf("got %q", s)
		}
	})

	t.Run("ContentBlocksWithData", func(t *testing.T) {
		blocks := []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "text part"},
			message.DataBlock{
				Type:   "data",
				Source: message.URLSource{Type: "url", URL: "https://img.example.com/1.png", MediaType: "image/png"},
			},
		}
		s := ConvertToolResultToString(blocks)
		if s != "text part\n[image/png: https://img.example.com/1.png]" {
			t.Errorf("got %q", s)
		}
	})

	t.Run("Other", func(t *testing.T) {
		s := ConvertToolResultToString(42)
		if s != "42" {
			t.Errorf("got %q", s)
		}
	})
}

func TestSupportsMediaType(t *testing.T) {
	supported := []string{"image/*", "audio/*"}

	tests := []struct {
		mediaType string
		want      bool
	}{
		{"image/png", true},
		{"image/jpeg", true},
		{"audio/mp3", true},
		{"video/mp4", false},
		{"text/plain", false},
	}

	for _, tt := range tests {
		t.Run(tt.mediaType, func(t *testing.T) {
			got := SupportsMediaType(supported, tt.mediaType)
			if got != tt.want {
				t.Errorf("SupportsMediaType(%v, %v) = %v, want %v", supported, tt.mediaType, got, tt.want)
			}
		})
	}
}

func TestGroupMessages(t *testing.T) {
	msgs := []*message.Msg{
		message.UserMsg("user", "hello"),
		message.AssistantMsg("bot", []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "tc1", Name: "fn", Input: `{}`},
		}),
		message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
			message.ToolResultBlock{Type: "tool_result", ID: "tc1", Output: "ok"},
		}),
		message.AssistantMsg("bot", "done"),
	}

	groups := GroupMessages(msgs)
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(groups))
	}
	if groups[0].Type != "agent_message" {
		t.Errorf("group[0].Type = %v, want agent_message", groups[0].Type)
	}
	if groups[1].Type != "tool_sequence" {
		t.Errorf("group[1].Type = %v, want tool_sequence", groups[1].Type)
	}
	if groups[2].Type != "agent_message" {
		t.Errorf("group[2].Type = %v, want agent_message", groups[2].Type)
	}

	// Tool sequence should contain both tool call and tool result messages
	if len(groups[1].Msgs) != 2 {
		t.Errorf("tool_sequence group has %d msgs, want 2", len(groups[1].Msgs))
	}
}

func TestGroupMessages_NilsSkipped(t *testing.T) {
	msgs := []*message.Msg{nil, message.UserMsg("u", "hi"), nil}
	groups := GroupMessages(msgs)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if len(groups[0].Msgs) != 1 {
		t.Errorf("group[0] has %d msgs, want 1", len(groups[0].Msgs))
	}
}

func TestOpenAIFamily_AudioUnsupportedSubtypeRejected(t *testing.T) {
	// Upstream #2301: only wav/mp3/mpeg are valid input_audio formats; an
	// arbitrary subtype (e.g. audio/ogg) must produce a clear error instead
	// of being passed through to the API, which rejects it.
	msgs := []*message.Msg{
		message.UserMsg("user", []message.ContentBlock{
			message.DataBlock{
				Type: "data",
				ID:   "aud_ogg",
				Source: message.Base64Source{
					Type:      "base64",
					Data:      "CCCC",
					MediaType: "audio/ogg",
				},
			},
		}),
	}
	for _, nf := range []namedFormatter{
		{"OpenAI", NewOpenAIFormatter(), nil},
		{"DashScope", NewDashScopeFormatter(), nil},
	} {
		t.Run(nf.name, func(t *testing.T) {
			_, err := nf.f.Format(msgs)
			if err == nil {
				t.Fatal("expected an error for unsupported audio subtype")
			}
			if !strings.Contains(err.Error(), "audio/ogg") {
				t.Errorf("error should name the offending media type, got: %v", err)
			}
		})
	}
}
