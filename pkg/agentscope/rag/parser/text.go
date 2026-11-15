package parser

import (
	"context"
	"fmt"
	"io"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/rag"
)

// TextParser parses plain text files into Document chunks.
type TextParser struct {
	Cfg ChunkConfig
}

// NewTextParser creates a TextParser with the given chunk configuration.
func NewTextParser(cfg ChunkConfig) *TextParser {
	return &TextParser{Cfg: cfg}
}

// SupportedExtensions returns the file extensions this parser handles.
func (p *TextParser) SupportedExtensions() []string {
	return []string{".txt", ".md", ".csv", ".log", ".json"}
}

// Parse reads all text from r, chunks it, and returns Documents with metadata.
func (p *TextParser) Parse(_ context.Context, r io.Reader, filename string) ([]rag.Document, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("text parser: read failed: %w", err)
	}

	text := string(data)
	if len(text) == 0 {
		return nil, nil
	}

	chunks := ChunkText(text, p.Cfg)
	docs := make([]rag.Document, 0, len(chunks))
	for i, chunk := range chunks {
		docs = append(docs, rag.Document{
			ID:      fmt.Sprintf("%s_chunk_%d", filename, i),
			Content: chunk,
			Meta: map[string]any{
				"filename":    filename,
				"chunk_index": i,
			},
		})
	}
	return docs, nil
}
