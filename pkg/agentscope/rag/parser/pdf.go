package parser

import (
	"bytes"
	"compress/flate"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/rag"
)

// PDFParser extracts text from PDF files using a basic stream-based approach.
// It handles FlateDecode compressed streams and plain text streams.
// Complex or encrypted PDFs will return an error suggesting external tools.
type PDFParser struct {
	Cfg ChunkConfig
}

// NewPDFParser creates a PDFParser with the given chunk configuration.
func NewPDFParser(cfg ChunkConfig) *PDFParser {
	return &PDFParser{Cfg: cfg}
}

// SupportedExtensions returns the file extensions this parser handles.
func (p *PDFParser) SupportedExtensions() []string {
	return []string{".pdf"}
}

// Parse reads a PDF from r, extracts text from streams, and returns Document chunks.
func (p *PDFParser) Parse(_ context.Context, r io.Reader, filename string) ([]rag.Document, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("pdf parser: read failed: %w", err)
	}

	// Basic validation
	if !bytes.HasPrefix(data, []byte("%PDF")) {
		return nil, fmt.Errorf("pdf parser: not a valid PDF file")
	}

	// Check for encryption
	if bytes.Contains(data, []byte("/Encrypt")) {
		return nil, fmt.Errorf("pdf parser: encrypted PDF detected; use an external tool (e.g., pdftotext) for extraction")
	}

	text := extractPDFText(data)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("pdf parser: no extractable text found; the PDF may use image-based content or complex encoding; use an external tool (e.g., pdftotext) for extraction")
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

// extractPDFText finds all stream/endstream pairs and attempts to decode them.
func extractPDFText(data []byte) string {
	var sb strings.Builder
	streamMarker := []byte("stream")
	endstreamMarker := []byte("endstream")

	offset := 0
	for offset < len(data) {
		idx := bytes.Index(data[offset:], streamMarker)
		if idx < 0 {
			break
		}
		streamStart := offset + idx + len(streamMarker)

		// Skip \r\n or \n after "stream"
		if streamStart < len(data) && data[streamStart] == '\r' {
			streamStart++
		}
		if streamStart < len(data) && data[streamStart] == '\n' {
			streamStart++
		}

		endIdx := bytes.Index(data[streamStart:], endstreamMarker)
		if endIdx < 0 {
			break
		}
		streamData := data[streamStart : streamStart+endIdx]
		offset = streamStart + endIdx + len(endstreamMarker)

		// Check for FlateDecode in the preceding dictionary
		dictStart := offset - len(streamData) - len(endstreamMarker) - len(streamMarker) - 200
		if dictStart < 0 {
			dictStart = 0
		}
		dictRegion := data[dictStart : offset-len(streamData)-len(endstreamMarker)]
		isFlate := bytes.Contains(dictRegion, []byte("/FlateDecode"))

		var decoded []byte
		if isFlate {
			decoded = deflateStream(streamData)
		} else {
			decoded = streamData
		}

		if len(decoded) == 0 {
			continue
		}

		extracted := extractTextOperators(decoded)
		if extracted != "" {
			sb.WriteString(extracted)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// deflateStream attempts to decompress flate-encoded data.
func deflateStream(data []byte) []byte {
	reader := flate.NewReader(bytes.NewReader(data))
	defer reader.Close()

	var buf bytes.Buffer
	_, err := io.Copy(&buf, reader)
	if err != nil {
		return nil
	}
	return buf.Bytes()
}

// extractTextOperators pulls text from PDF content stream text operators.
func extractTextOperators(stream []byte) string {
	var sb strings.Builder
	content := string(stream)
	lines := strings.Split(content, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Handle Tj operator: (text) Tj
		if strings.HasSuffix(line, "Tj") || strings.HasSuffix(line, "TJ") {
			text := extractParenText(line)
			if text != "" {
				sb.WriteString(text)
			}
		}
		// Handle text show with line break operators
		if strings.HasSuffix(line, "'") {
			text := extractParenText(line)
			if text != "" {
				sb.WriteString(text)
				sb.WriteString("\n")
			}
		}
	}
	return sb.String()
}

// extractParenText extracts text between parentheses, handling escape sequences.
func extractParenText(s string) string {
	var sb strings.Builder
	depth := 0
	escaped := false

	for _, ch := range s {
		if escaped {
			switch ch {
			case 'n':
				sb.WriteRune('\n')
			case 'r':
				sb.WriteRune('\r')
			case 't':
				sb.WriteRune('\t')
			default:
				sb.WriteRune(ch)
			}
			escaped = false
			continue
		}
		if ch == '\\' && depth > 0 {
			escaped = true
			continue
		}
		if ch == '(' {
			if depth > 0 {
				sb.WriteRune(ch)
			}
			depth++
			continue
		}
		if ch == ')' {
			depth--
			if depth > 0 {
				sb.WriteRune(ch)
			}
			continue
		}
		if depth > 0 {
			sb.WriteRune(ch)
		}
	}
	return sb.String()
}
