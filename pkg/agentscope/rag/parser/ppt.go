package parser

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/rag"
)

// PPTParser parses .pptx files (Office Open XML Presentation) into Document chunks.
// It extracts text from <a:t> tags in each slide's XML.
type PPTParser struct {
	Cfg ChunkConfig
}

// NewPPTParser creates a PPTParser with the given chunk configuration.
func NewPPTParser(cfg ChunkConfig) *PPTParser {
	return &PPTParser{Cfg: cfg}
}

// SupportedExtensions returns the file extensions this parser handles.
func (p *PPTParser) SupportedExtensions() []string {
	return []string{".pptx"}
}

// Parse reads a .pptx file from r, extracts slide text, and returns Document chunks.
func (p *PPTParser) Parse(_ context.Context, r io.Reader, filename string) ([]rag.Document, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("ppt parser: read failed: %w", err)
	}

	zipReader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("ppt parser: invalid zip archive: %w", err)
	}

	// Find and sort slide files
	type slideEntry struct {
		number int
		file   *zip.File
	}
	var slides []slideEntry
	for _, f := range zipReader.File {
		if strings.HasPrefix(f.Name, "ppt/slides/slide") && strings.HasSuffix(f.Name, ".xml") {
			// Extract slide number from filename
			numStr := f.Name[len("ppt/slides/slide") : len(f.Name)-len(".xml")]
			num, err := strconv.Atoi(numStr)
			if err != nil {
				continue
			}
			slides = append(slides, slideEntry{number: num, file: f})
		}
	}

	sort.Slice(slides, func(i, j int) bool {
		return slides[i].number < slides[j].number
	})

	if len(slides) == 0 {
		return nil, fmt.Errorf("ppt parser: no slides found in archive")
	}

	var docs []rag.Document
	chunkIdx := 0
	for _, slide := range slides {
		text, err := extractSlideText(slide.file)
		if err != nil {
			continue
		}
		if strings.TrimSpace(text) == "" {
			continue
		}

		chunks := ChunkText(text, p.Cfg)
		for _, chunk := range chunks {
			docs = append(docs, rag.Document{
				ID:      fmt.Sprintf("%s_slide%d_chunk_%d", filename, slide.number, chunkIdx),
				Content: chunk,
				Meta: map[string]any{
					"filename":     filename,
					"slide_number": slide.number,
					"chunk_index":  chunkIdx,
				},
			})
			chunkIdx++
		}
	}

	if len(docs) == 0 {
		return nil, nil
	}
	return docs, nil
}

// extractSlideText reads a slide XML file and extracts all text from <a:t> elements.
func extractSlideText(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	var sb strings.Builder
	decoder := xml.NewDecoder(rc)
	inText := false

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
			// <a:t> contains the actual text
			if t.Name.Local == "t" && t.Name.Space == "http://schemas.openxmlformats.org/drawingml/2006/main" {
				inText = true
			}
		case xml.EndElement:
			if t.Name.Local == "t" && t.Name.Space == "http://schemas.openxmlformats.org/drawingml/2006/main" {
				inText = false
				sb.WriteString(" ")
			}
			// Add newline at paragraph end
			if t.Name.Local == "p" && t.Name.Space == "http://schemas.openxmlformats.org/drawingml/2006/main" {
				sb.WriteString("\n")
			}
		case xml.CharData:
			if inText {
				sb.Write(t)
			}
		}
	}
	return sb.String(), nil
}
