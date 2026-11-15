package parser

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/rag"
)

// Parser converts a file into a slice of Documents suitable for indexing.
type Parser interface {
	Parse(ctx context.Context, r io.Reader, filename string) ([]rag.Document, error)
	SupportedExtensions() []string
}

// ChunkUnit selects how MaxChunkSize/Overlap are measured.
type ChunkUnit string

const (
	// ChunkUnitChars measures chunk bounds in characters (default, and the
	// historical behavior).
	ChunkUnitChars ChunkUnit = "chars"
	// ChunkUnitApproxTokens measures chunk bounds in APPROXIMATE tokens
	// (upstream #2083: token-oriented chunking without a tokenizer
	// dependency — one token is estimated as approxTokenChars characters).
	ChunkUnitApproxTokens ChunkUnit = "approx_tokens"
)

// approxTokenChars is the characters-per-token estimate used by
// ChunkUnitApproxTokens. 4 is the common rule of thumb for English text with
// modern BPE tokenizers.
//
// It UNDER-counts CJK text: one token is roughly one to one-and-a-half
// Chinese characters, not four, so a chunk sized "1000 approx tokens" holds
// about 1000 characters but closer to 700-1000 real tokens — i.e. CJK chunks
// come out LARGER in token terms than configured, not smaller. Callers with
// CJK-heavy corpora and a hard token ceiling should set MaxChunkSize lower
// (or use ChunkUnitChars and size it themselves) rather than rely on this
// estimate.
const approxTokenChars = 4

// ChunkConfig controls text chunking behavior. Upstream #2083 makes the
// configuration explicit and validatable: callers can Validate() a config
// (e.g. at an API boundary) instead of relying on silent fallbacks.
type ChunkConfig struct {
	MaxChunkSize int       // max size per chunk, in Unit (default 1000 chars)
	Overlap      int       // overlap between consecutive chunks, in Unit (default 200)
	Unit         ChunkUnit // measurement unit; empty means ChunkUnitChars
}

// DefaultChunkConfig returns sensible chunking defaults.
func DefaultChunkConfig() ChunkConfig {
	return ChunkConfig{
		MaxChunkSize: 1000,
		Overlap:      200,
		Unit:         ChunkUnitChars,
	}
}

// Validate reports whether the config is internally consistent. Zero values
// are legal in ChunkText (defaults apply), but an API that accepts a config
// from a client should reject nonsense explicitly rather than fold it into
// a default (upstream #2083, same spirit as #2442 for cron).
func (c ChunkConfig) Validate() error {
	switch c.Unit {
	case "", ChunkUnitChars, ChunkUnitApproxTokens:
	default:
		return fmt.Errorf("chunk config: unknown unit %q", c.Unit)
	}
	if c.MaxChunkSize < 0 {
		return fmt.Errorf("chunk config: max_chunk_size must be >= 0, got %d", c.MaxChunkSize)
	}
	if c.Overlap < 0 {
		return fmt.Errorf("chunk config: overlap must be >= 0, got %d", c.Overlap)
	}
	if c.MaxChunkSize > 0 && c.Overlap >= c.MaxChunkSize {
		return fmt.Errorf("chunk config: overlap %d must be smaller than max_chunk_size %d", c.Overlap, c.MaxChunkSize)
	}
	return nil
}

// charBounds converts the configured size/overlap into character counts.
func (c ChunkConfig) charBounds() (int, int) {
	size, overlap := c.MaxChunkSize, c.Overlap
	if c.Unit == ChunkUnitApproxTokens {
		size *= approxTokenChars
		overlap *= approxTokenChars
	}
	return size, overlap
}

// ChunkText splits text into overlapping chunks according to cfg.
// If text is shorter than MaxChunkSize, a single chunk is returned.
func ChunkText(text string, cfg ChunkConfig) []string {
	if cfg.MaxChunkSize <= 0 {
		cfg.MaxChunkSize = 1000
		if cfg.Unit == ChunkUnitApproxTokens {
			cfg.MaxChunkSize = 250 // 1000 chars worth of approximate tokens
		}
	}
	if cfg.Overlap < 0 {
		cfg.Overlap = 0
	}
	if cfg.Overlap >= cfg.MaxChunkSize {
		cfg.Overlap = cfg.MaxChunkSize / 2
	}
	// Approx-token mode: run the same sliding window over character counts
	// derived from the token estimates (upstream #2083).
	if maxChars, overlapChars := cfg.charBounds(); cfg.Unit == ChunkUnitApproxTokens {
		cfg.MaxChunkSize, cfg.Overlap = maxChars, overlapChars
		cfg.Unit = ChunkUnitChars
	}

	runes := []rune(text)
	total := len(runes)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if total <= cfg.MaxChunkSize {
		return []string{string(runes)}
	}

	var chunks []string
	step := cfg.MaxChunkSize - cfg.Overlap
	if step <= 0 {
		step = 1
	}

	for start := 0; start < total; start += step {
		end := start + cfg.MaxChunkSize
		if end > total {
			end = total
		}
		chunk := string(runes[start:end])
		if strings.TrimSpace(chunk) != "" {
			chunks = append(chunks, chunk)
		}
		if end == total {
			break
		}
	}
	return chunks
}
