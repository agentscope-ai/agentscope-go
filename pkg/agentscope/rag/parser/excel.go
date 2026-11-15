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

// ExcelParser parses .xlsx files (Office Open XML Spreadsheet) into Document chunks.
// It reads shared strings and sheet data from the ZIP archive.
type ExcelParser struct {
	Cfg ChunkConfig
}

// NewExcelParser creates an ExcelParser with the given chunk configuration.
func NewExcelParser(cfg ChunkConfig) *ExcelParser {
	return &ExcelParser{Cfg: cfg}
}

// SupportedExtensions returns the file extensions this parser handles.
func (p *ExcelParser) SupportedExtensions() []string {
	return []string{".xlsx"}
}

// Parse reads an .xlsx file from r, extracts cell text, and returns Document chunks.
func (p *ExcelParser) Parse(_ context.Context, r io.Reader, filename string) ([]rag.Document, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("excel parser: read failed: %w", err)
	}

	zipReader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("excel parser: invalid zip archive: %w", err)
	}

	// Read shared strings
	sharedStrings, err := readSharedStrings(zipReader)
	if err != nil {
		// Not all xlsx files have shared strings; proceed with empty table
		sharedStrings = nil
	}

	// Read sheet1 data
	sheetText, err := readSheet(zipReader, sharedStrings)
	if err != nil {
		return nil, fmt.Errorf("excel parser: failed to read sheet data: %w", err)
	}

	if strings.TrimSpace(sheetText) == "" {
		return nil, nil
	}

	chunks := ChunkText(sheetText, p.Cfg)
	docs := make([]rag.Document, 0, len(chunks))
	for i, chunk := range chunks {
		docs = append(docs, rag.Document{
			ID:      fmt.Sprintf("%s_chunk_%d", filename, i),
			Content: chunk,
			Meta: map[string]any{
				"filename":    filename,
				"sheet_name":  "Sheet1",
				"chunk_index": i,
			},
		})
	}
	return docs, nil
}

// xlsxSST represents the shared string table.
type xlsxSST struct {
	SI []xlsxSI `xml:"si"`
}

type xlsxSI struct {
	T string  `xml:"t"`
	R []xlsxR `xml:"r"`
}

type xlsxR struct {
	T string `xml:"t"`
}

// readSharedStrings parses xl/sharedStrings.xml from the ZIP.
func readSharedStrings(zr *zip.Reader) ([]string, error) {
	var file *zip.File
	for _, f := range zr.File {
		if f.Name == "xl/sharedStrings.xml" {
			file = f
			break
		}
	}
	if file == nil {
		return nil, fmt.Errorf("sharedStrings.xml not found")
	}

	rc, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	var sst xlsxSST
	if err := xml.NewDecoder(rc).Decode(&sst); err != nil {
		return nil, err
	}

	strings := make([]string, 0, len(sst.SI))
	for _, si := range sst.SI {
		if si.T != "" {
			strings = append(strings, si.T)
		} else {
			// Concatenate rich text runs
			var sb bytes.Buffer
			for _, r := range si.R {
				sb.WriteString(r.T)
			}
			strings = append(strings, sb.String())
		}
	}
	return strings, nil
}

// xlsxWorksheet represents the worksheet XML structure.
type xlsxWorksheet struct {
	SheetData xlsxSheetData `xml:"sheetData"`
}

type xlsxSheetData struct {
	Rows []xlsxRow `xml:"row"`
}

type xlsxRow struct {
	Cells []xlsxCell `xml:"c"`
}

type xlsxCell struct {
	Type  string `xml:"t,attr"`
	Value string `xml:"v"`
}

// readSheet parses xl/worksheets/sheet1.xml and converts rows to tab-separated text.
func readSheet(zr *zip.Reader, sharedStrings []string) (string, error) {
	var file *zip.File
	for _, f := range zr.File {
		if f.Name == "xl/worksheets/sheet1.xml" {
			file = f
			break
		}
	}
	if file == nil {
		return "", fmt.Errorf("sheet1.xml not found")
	}

	rc, err := file.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	var ws xlsxWorksheet
	if err := xml.NewDecoder(rc).Decode(&ws); err != nil {
		return "", err
	}

	var sb strings.Builder
	for _, row := range ws.SheetData.Rows {
		var cells []string
		for _, cell := range row.Cells {
			val := cell.Value
			// If type is "s", the value is an index into shared strings
			if cell.Type == "s" && sharedStrings != nil {
				idx := 0
				for _, ch := range val {
					idx = idx*10 + int(ch-'0')
				}
				if idx >= 0 && idx < len(sharedStrings) {
					val = sharedStrings[idx]
				}
			}
			cells = append(cells, val)
		}
		sb.WriteString(strings.Join(cells, "\t"))
		sb.WriteString("\n")
	}
	return sb.String(), nil
}
