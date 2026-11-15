package parser

import (
	"strings"
	"testing"
)

// Upstream #2083: explicit, validatable chunker configuration.
func TestChunkConfigValidate(t *testing.T) {
	valid := []ChunkConfig{
		DefaultChunkConfig(),
		{MaxChunkSize: 0, Overlap: 0}, // zero → defaults at chunk time
		{MaxChunkSize: 100, Overlap: 10, Unit: ChunkUnitApproxTokens},
		{MaxChunkSize: 100, Overlap: 10, Unit: ChunkUnitChars},
		{MaxChunkSize: 100, Overlap: 10, Unit: ""},
	}
	for _, c := range valid {
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", c, err)
		}
	}
	invalid := []ChunkConfig{
		{MaxChunkSize: -1},
		{Overlap: -5},
		{MaxChunkSize: 100, Overlap: 100},
		{MaxChunkSize: 100, Overlap: 200},
		{Unit: ChunkUnit("words")},
	}
	for _, c := range invalid {
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want error", c)
		}
	}
}

func TestChunkTextApproxTokens(t *testing.T) {
	text := strings.Repeat("word ", 400) // 2000 chars ≈ 500 approx tokens
	chunks := ChunkText(text, ChunkConfig{MaxChunkSize: 50, Overlap: 5, Unit: ChunkUnitApproxTokens})
	if len(chunks) < 2 {
		t.Fatalf("approx-token chunking produced %d chunks, want several", len(chunks))
	}
	for _, c := range chunks {
		// 50 approx tokens ≈ 200 chars, allow the final-chunk slack.
		if len(c) > 200+len("word ") {
			t.Errorf("chunk of %d chars exceeds the ~200-char budget", len(c))
		}
	}
	// Same text in char mode with the equivalent budget behaves identically.
	charChunks := ChunkText(text, ChunkConfig{MaxChunkSize: 200, Overlap: 20, Unit: ChunkUnitChars})
	if len(charChunks) != len(chunks) {
		t.Errorf("approx-token chunks = %d, char-equivalent = %d, want equal", len(chunks), len(charChunks))
	}
}

func TestChunkTextCharModeUnchanged(t *testing.T) {
	text := strings.Repeat("abcdefghij", 100) // 1000 chars
	chunks := ChunkText(text, DefaultChunkConfig())
	if len(chunks) != 1 {
		t.Errorf("1000 chars with default 1000-char chunks = %d chunks, want 1", len(chunks))
	}
	chunks = ChunkText(text, ChunkConfig{MaxChunkSize: 300, Overlap: 100})
	if len(chunks) < 4 {
		t.Errorf("300/100 over 1000 chars = %d chunks, want >= 4", len(chunks))
	}
}
