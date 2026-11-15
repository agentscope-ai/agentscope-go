package message

import (
	"encoding/json"
	"testing"
)

func TestNewMsg_String(t *testing.T) {
	msg := NewMsg("alice", RoleUser, "hello")
	if msg.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if msg.Name != "alice" {
		t.Fatalf("expected name alice, got %s", msg.Name)
	}
	if msg.Role != RoleUser {
		t.Fatalf("expected role user, got %s", msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(msg.Content))
	}
	tb, ok := msg.Content[0].(TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", msg.Content[0])
	}
	if tb.Text != "hello" {
		t.Fatalf("expected text hello, got %s", tb.Text)
	}
	if tb.ID == "" {
		t.Fatal("expected non-empty block ID")
	}
}

func TestNewMsg_ContentBlocks(t *testing.T) {
	blocks := []ContentBlock{
		TextBlock{Type: "text", ID: "t1", Text: "hello"},
		ThinkingBlock{Type: "thinking", ID: "th1", Thinking: "reasoning"},
	}
	msg := NewMsg("bot", RoleAssistant, blocks)
	if len(msg.Content) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(msg.Content))
	}
	if msg.Content[0].GetType() != ContentBlockText {
		t.Fatalf("expected text block, got %s", msg.Content[0].GetType())
	}
	if msg.Content[1].GetType() != ContentBlockThinking {
		t.Fatalf("expected thinking block, got %s", msg.Content[1].GetType())
	}
}

func TestNewMsg_InvalidContent(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for invalid content type")
		}
	}()
	NewMsg("test", RoleUser, 42)
}

func TestUserMsg(t *testing.T) {
	msg := UserMsg("alice", "hi")
	if msg.Role != RoleUser {
		t.Fatalf("expected user role, got %s", msg.Role)
	}
	if msg.FinishedAt == "" {
		t.Fatal("expected FinishedAt set for user msg")
	}
}

func TestUserMsg_InvalidBlock(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for ThinkingBlock in user msg")
		}
	}()
	UserMsg("alice", []ContentBlock{ThinkingBlock{Type: "thinking", ID: "t1", Thinking: "hmm"}})
}

func TestSystemMsg(t *testing.T) {
	msg := SystemMsg("sys", "you are helpful")
	if msg.Role != RoleSystem {
		t.Fatalf("expected system role, got %s", msg.Role)
	}
	if msg.FinishedAt == "" {
		t.Fatal("expected FinishedAt set for system msg")
	}
}

func TestSystemMsg_InvalidBlock(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for DataBlock in system msg")
		}
	}()
	SystemMsg("sys", []ContentBlock{DataBlock{Type: "data", ID: "d1", Source: Base64Source{Type: "base64", Data: "abc"}}})
}

func TestAssistantMsg(t *testing.T) {
	msg := AssistantMsg("bot", "reply")
	if msg.Role != RoleAssistant {
		t.Fatalf("expected assistant role, got %s", msg.Role)
	}
	if msg.FinishedAt != "" {
		t.Fatal("expected empty FinishedAt for assistant msg")
	}
}

func TestGetTextContent(t *testing.T) {
	msg := NewMsg("bot", RoleAssistant, []ContentBlock{
		TextBlock{Type: "text", ID: "1", Text: "hello"},
		ThinkingBlock{Type: "thinking", ID: "2", Thinking: "hmm"},
		TextBlock{Type: "text", ID: "3", Text: "world"},
	})
	result := msg.GetTextContent(" ")
	if result == nil {
		t.Fatal("expected non-nil text content")
	}
	if *result != "hello world" {
		t.Fatalf("expected 'hello world', got '%s'", *result)
	}
}

func TestGetTextContent_NoTextBlocks(t *testing.T) {
	msg := NewMsg("bot", RoleAssistant, []ContentBlock{
		ThinkingBlock{Type: "thinking", ID: "1", Thinking: "hmm"},
	})
	result := msg.GetTextContent(" ")
	if result != nil {
		t.Fatal("expected nil for no text blocks")
	}
}

func TestGetContentBlocks_Filtered(t *testing.T) {
	msg := NewMsg("bot", RoleAssistant, []ContentBlock{
		TextBlock{Type: "text", ID: "1", Text: "hello"},
		ToolCallBlock{Type: "tool_call", ID: "tc1", Name: "bash", Input: `{"cmd":"ls"}`, State: ToolCallPending},
		TextBlock{Type: "text", ID: "2", Text: "world"},
	})

	textBlocks := msg.GetContentBlocks(ContentBlockText)
	if len(textBlocks) != 2 {
		t.Fatalf("expected 2 text blocks, got %d", len(textBlocks))
	}

	toolBlocks := msg.GetContentBlocks(ContentBlockToolCall)
	if len(toolBlocks) != 1 {
		t.Fatalf("expected 1 tool call block, got %d", len(toolBlocks))
	}

	allBlocks := msg.GetContentBlocks()
	if len(allBlocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(allBlocks))
	}
}

func TestHasContentBlocks(t *testing.T) {
	msg := NewMsg("bot", RoleAssistant, []ContentBlock{
		TextBlock{Type: "text", ID: "1", Text: "hello"},
	})
	if !msg.HasContentBlocks(ContentBlockText) {
		t.Fatal("expected to have text blocks")
	}
	if msg.HasContentBlocks(ContentBlockToolCall) {
		t.Fatal("expected not to have tool call blocks")
	}
}

func TestToolCallBlock_ParseInput(t *testing.T) {
	tc := ToolCallBlock{
		Type:  "tool_call",
		ID:    "tc1",
		Name:  "bash",
		Input: `{"command":"ls -la","timeout":30}`,
		State: ToolCallPending,
	}
	args, err := tc.ParseInput()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if args["command"] != "ls -la" {
		t.Fatalf("expected command 'ls -la', got %v", args["command"])
	}
	if args["timeout"] != float64(30) {
		t.Fatalf("expected timeout 30, got %v", args["timeout"])
	}
}

func TestToolCallBlock_ParseInput_Empty(t *testing.T) {
	tc := ToolCallBlock{Type: "tool_call", ID: "tc1", Name: "test", Input: "", State: ToolCallPending}
	args, err := tc.ParseInput()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(args) != 0 {
		t.Fatalf("expected empty args, got %v", args)
	}
}

func TestToolResultBlock_GetOutputText(t *testing.T) {
	tr := ToolResultBlock{
		Type:   "tool_result",
		ID:     "tr1",
		Name:   "bash",
		Output: "file1.go\nfile2.go",
		State:  ToolResultSuccess,
	}
	if tr.GetOutputText() != "file1.go\nfile2.go" {
		t.Fatalf("expected string output, got '%s'", tr.GetOutputText())
	}
}

func TestDataBlock_GetMediaType(t *testing.T) {
	db := DataBlock{
		Type:   "data",
		ID:     "d1",
		Source: Base64Source{Type: "base64", Data: "abc", MediaType: "image/png"},
	}
	if db.GetMediaType() != "image/png" {
		t.Fatalf("expected image/png, got %s", db.GetMediaType())
	}

	dbURL := DataBlock{
		Type:   "data",
		ID:     "d2",
		Source: URLSource{Type: "url", URL: "https://example.com/img.jpg", MediaType: "image/jpeg"},
	}
	if dbURL.GetMediaType() != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %s", dbURL.GetMediaType())
	}
}

func TestHintBlock(t *testing.T) {
	hb := HintBlock{
		Type:   "hint",
		ID:     "h1",
		Source: "budget_middleware",
		Hint:   "Token budget exceeded. Provide final answer.",
	}
	if hb.GetType() != ContentBlockHint {
		t.Fatalf("expected hint type, got %s", hb.GetType())
	}
	if hb.Source != "budget_middleware" {
		t.Fatalf("expected source budget_middleware, got %s", hb.Source)
	}
}

func TestToMap(t *testing.T) {
	msg := UserMsg("alice", "hello")
	m := msg.ToMap()
	if m["name"] != "alice" {
		t.Fatalf("expected name alice, got %v", m["name"])
	}
	if m["role"] != "user" {
		t.Fatalf("expected role user, got %v", m["role"])
	}
}

func TestFindBlockByTypeAndID(t *testing.T) {
	msg := NewMsg("bot", RoleAssistant, []ContentBlock{
		TextBlock{Type: "text", ID: "t1", Text: "hello"},
		TextBlock{Type: "text", ID: "t2", Text: "world"},
	})
	idx := msg.findBlockByTypeAndID(ContentBlockText, "t2")
	if idx != 1 {
		t.Fatalf("expected index 1, got %d", idx)
	}
	idx = msg.findBlockByTypeAndID(ContentBlockText, "nonexistent")
	if idx != -1 {
		t.Fatalf("expected -1, got %d", idx)
	}
}

func TestUsage(t *testing.T) {
	msg := AssistantMsg("bot", "reply")
	msg.Usage = &Usage{InputTokens: 100, OutputTokens: 50}
	if msg.Usage.InputTokens != 100 {
		t.Fatalf("expected 100 input tokens, got %d", msg.Usage.InputTokens)
	}
}

// --- JSON round-trip tests ---

func TestMsgJSON_TextBlock(t *testing.T) {
	msg := UserMsg("alice", "hello world")
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	var restored Msg
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	if restored.Name != "alice" {
		t.Errorf("Name = %s, want alice", restored.Name)
	}
	if restored.Role != RoleUser {
		t.Errorf("Role = %s, want user", restored.Role)
	}
	if len(restored.Content) != 1 {
		t.Fatalf("Content len = %d, want 1", len(restored.Content))
	}
	tb, ok := restored.Content[0].(TextBlock)
	if !ok {
		t.Fatalf("Content[0] type = %T, want TextBlock", restored.Content[0])
	}
	if tb.Text != "hello world" {
		t.Errorf("Text = %s, want 'hello world'", tb.Text)
	}
}

func TestMsgJSON_ToolCallBlock(t *testing.T) {
	msg := NewMsg("bot", RoleAssistant, []ContentBlock{
		ToolCallBlock{
			Type:  "tool_call",
			ID:    "tc1",
			Name:  "search",
			Input: `{"query":"test"}`,
			State: ToolCallPending,
		},
	})

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	var restored Msg
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	if len(restored.Content) != 1 {
		t.Fatalf("Content len = %d, want 1", len(restored.Content))
	}
	tc, ok := restored.Content[0].(ToolCallBlock)
	if !ok {
		t.Fatalf("Content[0] type = %T, want ToolCallBlock", restored.Content[0])
	}
	if tc.Name != "search" {
		t.Errorf("Name = %s, want search", tc.Name)
	}
	if tc.Input != `{"query":"test"}` {
		t.Errorf("Input = %s", tc.Input)
	}
	if tc.State != ToolCallPending {
		t.Errorf("State = %s, want pending", tc.State)
	}
}

func TestMsgJSON_ToolResultBlock(t *testing.T) {
	msg := NewMsg("bot", RoleAssistant, []ContentBlock{
		ToolResultBlock{
			Type:   "tool_result",
			ID:     "tr1",
			Name:   "search",
			Output: "found 5 results",
			State:  ToolResultSuccess,
		},
	})

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	var restored Msg
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	tr, ok := restored.Content[0].(ToolResultBlock)
	if !ok {
		t.Fatalf("Content[0] type = %T, want ToolResultBlock", restored.Content[0])
	}
	if tr.Output != "found 5 results" {
		t.Errorf("Output = %v, want 'found 5 results'", tr.Output)
	}
}

func TestMsgJSON_ThinkingBlock(t *testing.T) {
	msg := NewMsg("bot", RoleAssistant, []ContentBlock{
		ThinkingBlock{Type: "thinking", ID: "th1", Thinking: "Let me think..."},
	})

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	var restored Msg
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	th, ok := restored.Content[0].(ThinkingBlock)
	if !ok {
		t.Fatalf("Content[0] type = %T, want ThinkingBlock", restored.Content[0])
	}
	if th.Thinking != "Let me think..." {
		t.Errorf("Thinking = %s", th.Thinking)
	}
}

func TestMsgJSON_HintBlock(t *testing.T) {
	msg := NewMsg("sys", RoleSystem, []ContentBlock{
		HintBlock{Type: "hint", ID: "h1", Source: "budget", Hint: "tokens remaining: 1000"},
	})

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	var restored Msg
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	hb, ok := restored.Content[0].(HintBlock)
	if !ok {
		t.Fatalf("Content[0] type = %T, want HintBlock", restored.Content[0])
	}
	if hb.Hint != "tokens remaining: 1000" {
		t.Errorf("Hint = %s", hb.Hint)
	}
}

func TestMsgJSON_DataBlock_Base64(t *testing.T) {
	msg := NewMsg("user", RoleUser, []ContentBlock{
		DataBlock{
			Type: "data",
			ID:   "d1",
			Name: "image.png",
			Source: Base64Source{
				Type:      "base64",
				Data:      "iVBORw0KGgo=",
				MediaType: "image/png",
			},
		},
	})

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	var restored Msg
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	db, ok := restored.Content[0].(DataBlock)
	if !ok {
		t.Fatalf("Content[0] type = %T, want DataBlock", restored.Content[0])
	}
	src, ok := db.Source.(Base64Source)
	if !ok {
		t.Fatalf("Source type = %T, want Base64Source", db.Source)
	}
	if src.MediaType != "image/png" {
		t.Errorf("MediaType = %s", src.MediaType)
	}
}

func TestMsgJSON_DataBlock_URL(t *testing.T) {
	msg := NewMsg("user", RoleUser, []ContentBlock{
		DataBlock{
			Type: "data",
			ID:   "d2",
			Source: URLSource{
				Type:      "url",
				URL:       "https://example.com/img.jpg",
				MediaType: "image/jpeg",
			},
		},
	})

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	var restored Msg
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	db, ok := restored.Content[0].(DataBlock)
	if !ok {
		t.Fatalf("Content[0] type = %T, want DataBlock", restored.Content[0])
	}
	src, ok := db.Source.(URLSource)
	if !ok {
		t.Fatalf("Source type = %T, want URLSource", db.Source)
	}
	if src.URL != "https://example.com/img.jpg" {
		t.Errorf("URL = %s", src.URL)
	}
}

func TestMsgJSON_WithUsage(t *testing.T) {
	msg := AssistantMsg("bot", "reply")
	msg.Usage = &Usage{InputTokens: 100, OutputTokens: 50}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	var restored Msg
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	if restored.Usage == nil {
		t.Fatal("Usage should not be nil")
	}
	if restored.Usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", restored.Usage.InputTokens)
	}
}

func TestMsgJSON_MultipleBlocks(t *testing.T) {
	msg := NewMsg("bot", RoleAssistant, []ContentBlock{
		TextBlock{Type: "text", ID: "t1", Text: "Here's what I found:"},
		ToolCallBlock{Type: "tool_call", ID: "tc1", Name: "search", Input: `{}`, State: ToolCallPending},
	})

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	var restored Msg
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	if len(restored.Content) != 2 {
		t.Fatalf("Content len = %d, want 2", len(restored.Content))
	}
	if _, ok := restored.Content[0].(TextBlock); !ok {
		t.Errorf("Content[0] type = %T, want TextBlock", restored.Content[0])
	}
	if _, ok := restored.Content[1].(ToolCallBlock); !ok {
		t.Errorf("Content[1] type = %T, want ToolCallBlock", restored.Content[1])
	}
}
