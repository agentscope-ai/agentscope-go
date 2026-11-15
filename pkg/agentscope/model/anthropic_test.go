package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestApplyPromptCachingOff — with caching disabled, System stays a plain
// string and Tools remain unchanged. Existing wire format is preserved so
// nothing changes for callers who do not opt in.
func TestApplyPromptCachingOff(t *testing.T) {
	m := &AnthropicChatModel{promptCaching: false}
	tools := []anthropicTool{{Name: "A"}, {Name: "B"}}
	sysVal, outTools := m.applyPromptCaching("hi", tools)
	if s, ok := sysVal.(string); !ok || s != "hi" {
		t.Fatalf("System should stay string, got %T=%v", sysVal, sysVal)
	}
	if len(outTools) != 2 || outTools[1].CacheControl != nil {
		t.Fatalf("Tools should be untouched, got %+v", outTools)
	}
}

// TestApplyPromptCachingOn — with caching enabled, System becomes an array of
// text blocks with cache_control on the last block, and the last tool gets
// cache_control so Anthropic caches the tool-definitions prefix.
func TestApplyPromptCachingOn(t *testing.T) {
	m := &AnthropicChatModel{promptCaching: true}
	tools := []anthropicTool{{Name: "A"}, {Name: "B"}}
	sysVal, outTools := m.applyPromptCaching("YOU ARE X", tools)
	arr, ok := sysVal.([]anthropicSystemBlock)
	if !ok || len(arr) != 1 {
		t.Fatalf("System should be []anthropicSystemBlock of len 1, got %T=%v", sysVal, sysVal)
	}
	if arr[0].Type != "text" || arr[0].Text != "YOU ARE X" {
		t.Fatalf("system block content: %+v", arr[0])
	}
	if arr[0].CacheControl == nil || arr[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("cache_control missing on system: %+v", arr[0])
	}
	if outTools[0].CacheControl != nil {
		t.Fatal("only the last tool should be cached")
	}
	if outTools[1].CacheControl == nil || outTools[1].CacheControl.Type != "ephemeral" {
		t.Fatalf("cache_control missing on last tool: %+v", outTools[1])
	}
}

// TestApplyPromptCachingEmptySystem — with caching enabled but no system
// prompt, System serializes to nothing (not a stray empty block).
func TestApplyPromptCachingEmptySystem(t *testing.T) {
	m := &AnthropicChatModel{promptCaching: true}
	sysVal, _ := m.applyPromptCaching("", nil)
	if sysVal != nil {
		t.Fatalf("empty system with caching should stay nil, got %v", sysVal)
	}
}

// TestPromptCachingJSONShape — end-to-end wire format check: the request body
// serializes into exactly what Anthropic expects (system as text-block array,
// cache_control:{type:ephemeral} on the marked blocks). Locks in the JSON so
// a future refactor cannot silently change what the API sees.
func TestPromptCachingJSONShape(t *testing.T) {
	m := &AnthropicChatModel{promptCaching: true}
	sysVal, tools := m.applyPromptCaching("SYS", []anthropicTool{{Name: "T", Description: "d", InputSchema: json.RawMessage(`{}`)}})
	req := anthropicRequest{Model: "m", MaxTokens: 100, System: sysVal, Tools: tools}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		`"system":[{"type":"text","text":"SYS","cache_control":{"type":"ephemeral"}}]`,
		`"cache_control":{"type":"ephemeral"}`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("body missing %q:\n%s", want, got)
		}
	}
}
