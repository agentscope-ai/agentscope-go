package model

import "testing"

func TestParseGeminiResponse_UsageCountsToolAndThoughtTokens(t *testing.T) {
	// Upstream #2406: tool-use prompt tokens belong to input, thought tokens
	// belong to output; cached-content tokens feed the cache accounting.
	resp := geminiResponse{
		Candidates: []geminiCandidate{{
			Content: geminiContent{Parts: []geminiPart{{Text: "hi"}}},
		}},
		UsageMetadata: &geminiUsage{
			PromptTokenCount:        10,
			CandidatesTokenCount:    5,
			TotalTokenCount:         24,
			ToolUsePromptTokenCount: 3,
			ThoughtsTokenCount:      2,
			CachedContentTokenCount: 4,
		},
	}
	parsed, err := parseGeminiResponse(resp)
	if err != nil {
		t.Fatalf("parseGeminiResponse: %v", err)
	}
	if parsed.Usage == nil {
		t.Fatal("usage is nil")
	}
	if parsed.Usage.InputTokens != 13 {
		t.Errorf("InputTokens = %d, want 13 (10 prompt + 3 tool-use)", parsed.Usage.InputTokens)
	}
	if parsed.Usage.OutputTokens != 7 {
		t.Errorf("OutputTokens = %d, want 7 (5 candidates + 2 thoughts)", parsed.Usage.OutputTokens)
	}
	if parsed.Usage.CacheInputTokens != 4 {
		t.Errorf("CacheInputTokens = %d, want 4", parsed.Usage.CacheInputTokens)
	}
}
