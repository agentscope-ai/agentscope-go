package parser

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"
)

func buildMinimalPptx(slideXML string) *bytes.Buffer {
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)
	f, _ := w.Create("ppt/slides/slide1.xml")
	f.Write([]byte(slideXML))
	w.Close()
	return buf
}

func TestPPTParser_ParseSimplePptx(t *testing.T) {
	slideXML := `<?xml version="1.0" encoding="UTF-8"?>
<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"
       xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">
  <p:cSld>
    <p:spTree>
      <p:sp>
        <p:txBody>
          <a:p>
            <a:r><a:t>Slide Text</a:t></a:r>
          </a:p>
        </p:txBody>
      </p:sp>
    </p:spTree>
  </p:cSld>
</p:sld>`

	buf := buildMinimalPptx(slideXML)
	p := NewPPTParser(ChunkConfig{MaxChunkSize: 1000, Overlap: 100})
	docs, err := p.Parse(context.Background(), bytes.NewReader(buf.Bytes()), "test.pptx")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("expected at least one document")
	}
	if !strings.Contains(docs[0].Content, "Slide Text") {
		t.Errorf("expected content to contain Slide Text, got %q", docs[0].Content)
	}
}

func TestPPTParser_EmptySlide(t *testing.T) {
	slideXML := `<?xml version="1.0" encoding="UTF-8"?>
<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"
       xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">
  <p:cSld>
    <p:spTree></p:spTree>
  </p:cSld>
</p:sld>`

	buf := buildMinimalPptx(slideXML)
	p := NewPPTParser(ChunkConfig{MaxChunkSize: 1000, Overlap: 100})
	docs, err := p.Parse(context.Background(), bytes.NewReader(buf.Bytes()), "empty.pptx")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if docs != nil {
		t.Fatalf("expected nil for empty slide, got %d docs", len(docs))
	}
}
