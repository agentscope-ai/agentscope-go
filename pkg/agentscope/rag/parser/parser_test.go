package parser

import (
	"testing"
)

func TestChunkText_ShorterThanMax(t *testing.T) {
	cfg := ChunkConfig{MaxChunkSize: 100, Overlap: 20}
	text := "Hello world"
	chunks := ChunkText(text, cfg)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != text {
		t.Fatalf("expected %q, got %q", text, chunks[0])
	}
}

func TestChunkText_ExactlyMaxSize(t *testing.T) {
	cfg := ChunkConfig{MaxChunkSize: 10, Overlap: 3}
	text := "0123456789" // exactly 10 characters
	chunks := ChunkText(text, cfg)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != text {
		t.Fatalf("expected %q, got %q", text, chunks[0])
	}
}

func TestChunkText_LongerThanMax_MultipleChunks(t *testing.T) {
	cfg := ChunkConfig{MaxChunkSize: 10, Overlap: 3}
	text := "abcdefghijklmnopqrst" // 20 characters
	chunks := ChunkText(text, cfg)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	// Each chunk should be at most MaxChunkSize
	for i, c := range chunks {
		if len([]rune(c)) > cfg.MaxChunkSize {
			t.Errorf("chunk %d exceeds MaxChunkSize: len=%d", i, len([]rune(c)))
		}
	}
}

func TestChunkText_OverlapVerification(t *testing.T) {
	cfg := ChunkConfig{MaxChunkSize: 10, Overlap: 4}
	text := "abcdefghijklmnopqrst" // 20 characters
	chunks := ChunkText(text, cfg)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}

	// Verify overlap: end of chunk N should appear at start of chunk N+1
	for i := 0; i < len(chunks)-1; i++ {
		runes0 := []rune(chunks[i])
		runes1 := []rune(chunks[i+1])
		// The overlap region is the last `Overlap` chars of chunk i
		overlapEnd := string(runes0[len(runes0)-cfg.Overlap:])
		overlapStart := string(runes1[:cfg.Overlap])
		if overlapEnd != overlapStart {
			t.Errorf("overlap mismatch between chunk %d and %d: end=%q, start=%q",
				i, i+1, overlapEnd, overlapStart)
		}
	}
}

func TestChunkText_EmptyText(t *testing.T) {
	cfg := ChunkConfig{MaxChunkSize: 100, Overlap: 20}
	chunks := ChunkText("", cfg)
	if chunks != nil {
		t.Fatalf("expected nil for empty text, got %v", chunks)
	}
}

func TestDefaultChunkConfig_SensibleValues(t *testing.T) {
	cfg := DefaultChunkConfig()
	if cfg.MaxChunkSize <= 0 {
		t.Errorf("MaxChunkSize should be > 0, got %d", cfg.MaxChunkSize)
	}
	if cfg.Overlap <= 0 {
		t.Errorf("Overlap should be > 0, got %d", cfg.Overlap)
	}
	if cfg.Overlap >= cfg.MaxChunkSize {
		t.Errorf("Overlap (%d) should be < MaxChunkSize (%d)", cfg.Overlap, cfg.MaxChunkSize)
	}
}
