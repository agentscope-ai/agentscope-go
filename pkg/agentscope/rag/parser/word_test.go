package parser

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"
)

func buildMinimalDocx(xmlContent string) *bytes.Buffer {
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)
	f, _ := w.Create("word/document.xml")
	f.Write([]byte(xmlContent))
	w.Close()
	return buf
}

func TestWordParser_ParseSimpleDocx(t *testing.T) {
	docXML := `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p>
      <w:r><w:t>Hello World</w:t></w:r>
    </w:p>
  </w:body>
</w:document>`

	buf := buildMinimalDocx(docXML)
	p := NewWordParser(ChunkConfig{MaxChunkSize: 1000, Overlap: 100})
	docs, err := p.Parse(context.Background(), bytes.NewReader(buf.Bytes()), "test.docx")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("expected at least one document")
	}
	if !strings.Contains(docs[0].Content, "Hello World") {
		t.Errorf("expected content to contain Hello World, got %q", docs[0].Content)
	}
}

func TestWordParserPreservesRunWhitespace(t *testing.T) {
	const fixture = `<w:document xmlns:w="WORD_NAMESPACE" xmlns:a="urn:other">
	<w:body>
		<w:p>
			<w:pPr><w:tabs><w:tab w:pos="720"/></w:tabs></w:pPr>
			<w:r><w:t>A</w:t><w:tab/><w:t>B</w:t><a:br/><w:br/><w:t>C</w:t></w:r>
			<w:r><w:instrText>DO NOT EMIT</w:instrText><w:t xml:space="preserve"> </w:t><w:t>D</w:t></w:r>
		</w:p>
		<w:p><w:r><w:t>E</w:t></w:r></w:p>
	</w:body>
	</w:document>`
	for _, tt := range []struct{ name, namespace string }{
		{"transitional", "http://schemas.openxmlformats.org/wordprocessingml/2006/main"},
		{"strict", "http://purl.oclc.org/ooxml/wordprocessingml/main"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			doc := buildMinimalDocx(strings.ReplaceAll(fixture, "WORD_NAMESPACE", tt.namespace))
			docs, err := NewWordParser(DefaultChunkConfig()).Parse(context.Background(), doc, "runs.docx")
			if err != nil || len(docs) != 1 || docs[0].Content != "A\tB\nC D\nE\n" {
				t.Fatalf("docs=%+v err=%v", docs, err)
			}
		})
	}
}

func TestWordParser_EmptyDocument(t *testing.T) {
	docXML := `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p></w:p>
  </w:body>
</w:document>`

	buf := buildMinimalDocx(docXML)
	p := NewWordParser(ChunkConfig{MaxChunkSize: 1000, Overlap: 100})
	docs, err := p.Parse(context.Background(), bytes.NewReader(buf.Bytes()), "empty.docx")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if docs != nil {
		t.Fatalf("expected nil for empty document, got %d docs", len(docs))
	}
}
