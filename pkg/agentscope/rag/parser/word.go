package parser

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/rag"
)

// WordParser parses .docx files (Office Open XML) into Document chunks.
// It opens the ZIP archive, reads word/document.xml, and extracts text content.
type WordParser struct {
	Cfg ChunkConfig
}

// NewWordParser creates a WordParser with the given chunk configuration.
func NewWordParser(cfg ChunkConfig) *WordParser {
	return &WordParser{Cfg: cfg}
}

// SupportedExtensions returns the file extensions this parser handles.
func (p *WordParser) SupportedExtensions() []string {
	return []string{".docx"}
}

// Parse reads a .docx file from r, extracts text, and returns Document chunks.
func (p *WordParser) Parse(_ context.Context, r io.Reader, filename string) ([]rag.Document, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("word parser: read failed: %w", err)
	}

	zipReader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("word parser: invalid zip archive: %w", err)
	}

	var docFile *zip.File
	for _, f := range zipReader.File {
		if f.Name == "word/document.xml" {
			docFile = f
			break
		}
	}
	if docFile == nil {
		return nil, fmt.Errorf("word parser: word/document.xml not found in archive")
	}

	rc, err := docFile.Open()
	if err != nil {
		return nil, fmt.Errorf("word parser: cannot open document.xml: %w", err)
	}
	defer rc.Close()

	text, err := extractDocxText(rc)
	if err != nil {
		return nil, fmt.Errorf("word parser: text extraction failed: %w", err)
	}

	if strings.TrimSpace(text) == "" {
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

// extractDocxText parses the XML and extracts text from <w:t> elements,
// inserting newlines at paragraph boundaries (<w:p>).
func extractDocxText(r io.Reader) (string, error) {
	var sb strings.Builder
	decoder := xml.NewDecoder(r)
	inParagraph := false
	paragraphHasText := false

	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			// w:p marks a paragraph boundary
			if t.Name.Local == "p" && t.Name.Space == "http://schemas.openxmlformats.org/wordprocessingml/2006/main" {
				if inParagraph && paragraphHasText {
					sb.WriteString("\n")
				}
				inParagraph = true
				paragraphHasText = false
			}
		case xml.CharData:
			text := strings.TrimSpace(string(t))
			if text != "" {
				sb.WriteString(string(t))
				paragraphHasText = true
			}
		}
	}
	if paragraphHasText {
		sb.WriteString("\n")
	}
	return sb.String(), nil
}
