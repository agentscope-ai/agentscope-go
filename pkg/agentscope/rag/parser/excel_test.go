package parser

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"
)

func buildMinimalXlsx(sharedStrings, sheet1 string) *bytes.Buffer {
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)
	if sharedStrings != "" {
		f, _ := w.Create("xl/sharedStrings.xml")
		f.Write([]byte(sharedStrings))
	}
	f, _ := w.Create("xl/worksheets/sheet1.xml")
	f.Write([]byte(sheet1))
	w.Close()
	return buf
}

func TestExcelParser_ParseSimpleXlsx(t *testing.T) {
	sharedStrings := `<?xml version="1.0" encoding="UTF-8"?>
<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="3" uniqueCount="3">
  <si><t>Name</t></si>
  <si><t>Age</t></si>
  <si><t>Alice</t></si>
</sst>`

	sheet1 := `<?xml version="1.0" encoding="UTF-8"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <sheetData>
    <row>
      <c t="s"><v>0</v></c>
      <c t="s"><v>1</v></c>
    </row>
    <row>
      <c t="s"><v>2</v></c>
      <c><v>30</v></c>
    </row>
  </sheetData>
</worksheet>`

	buf := buildMinimalXlsx(sharedStrings, sheet1)
	p := NewExcelParser(ChunkConfig{MaxChunkSize: 1000, Overlap: 100})
	docs, err := p.Parse(context.Background(), bytes.NewReader(buf.Bytes()), "data.xlsx")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("expected at least one document")
	}
	// Verify that row data is extracted (tab-separated)
	content := docs[0].Content
	if !strings.Contains(content, "Name") {
		t.Errorf("expected content to contain Name, got %q", content)
	}
	if !strings.Contains(content, "Alice") {
		t.Errorf("expected content to contain Alice, got %q", content)
	}
	if !strings.Contains(content, "30") {
		t.Errorf("expected content to contain 30, got %q", content)
	}
}

func TestExcelParser_EmptySheet(t *testing.T) {
	sheet1 := `<?xml version="1.0" encoding="UTF-8"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <sheetData></sheetData>
</worksheet>`

	buf := buildMinimalXlsx("", sheet1)
	p := NewExcelParser(ChunkConfig{MaxChunkSize: 1000, Overlap: 100})
	docs, err := p.Parse(context.Background(), bytes.NewReader(buf.Bytes()), "empty.xlsx")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if docs != nil {
		t.Fatalf("expected nil for empty sheet, got %d docs", len(docs))
	}
}
