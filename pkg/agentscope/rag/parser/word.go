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

// extractDocxText preserves text, tabs and breaks in runs, and separates paragraphs.
func extractDocxText(r io.Reader) (string, error) {
	const (
		transitionalNS = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
		strictNS       = "http://purl.oclc.org/ooxml/wordprocessingml/main"
	)
	isWordNamespace := func(space string) bool {
		return space == transitionalNS || space == strictNS
	}
	var sb strings.Builder
	var elements []xml.Name
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
			if isWordNamespace(t.Name.Space) {
				switch t.Name.Local {
				case "p":
					if inParagraph && paragraphHasText {
						sb.WriteByte('\n')
					}
					inParagraph = true
					paragraphHasText = false
				case "tab", "br":
					// A tab stop under paragraph properties is not run content.
					if len(elements) > 0 && elements[len(elements)-1] == (xml.Name{Space: t.Name.Space, Local: "r"}) {
						if t.Name.Local == "tab" {
							sb.WriteByte('\t')
						} else {
							sb.WriteByte('\n')
						}
						paragraphHasText = true
					}
				}
			}
			elements = append(elements, t.Name)
		case xml.EndElement:
			elements = elements[:len(elements)-1]
		case xml.CharData:
			if len(elements) < 2 {
				continue
			}
			name, parent := elements[len(elements)-1], elements[len(elements)-2]
			if name.Local == "t" && isWordNamespace(name.Space) && parent == (xml.Name{Space: name.Space, Local: "r"}) {
				sb.Write(t)
				paragraphHasText = true
			}
		}
	}
	if paragraphHasText {
		sb.WriteString("\n")
	}
	return sb.String(), nil
}
