package parser

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
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

func TestPPTParser_DrawingMLNamespaces(t *testing.T) {
	const slide = `<p:sld xmlns:p="%s" xmlns:d="%s" xmlns:ext="urn:fixture">
  <p:cSld><p:spTree><p:sp><p:txBody>
    <d:p><d:r><d:t>First paragraph</d:t></d:r></d:p>
    <d:p><d:r><d:t>Second paragraph</d:t></d:r></d:p>
  </p:txBody></p:sp></p:spTree></p:cSld>
  <p:extLst><p:ext uri="{8CBCB192-A3C8-44D5-A2BF-6C054B62C612}">
    <ext:p><ext:t>Ignored extension text</ext:t></ext:p>
  </p:ext></p:extLst>
</p:sld>`
	tests := []struct {
		name           string
		presentationNS string
		drawingNS      string
	}{
		{
			name:           "Transitional",
			presentationNS: "http://schemas.openxmlformats.org/presentationml/2006/main",
			drawingNS:      "http://schemas.openxmlformats.org/drawingml/2006/main",
		},
		{
			name:           "Strict",
			presentationNS: "http://purl.oclc.org/ooxml/presentationml/main",
			drawingNS:      "http://purl.oclc.org/ooxml/drawingml/main",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := buildMinimalPptx(fmt.Sprintf(slide, tt.presentationNS, tt.drawingNS))
			p := NewPPTParser(DefaultChunkConfig())
			docs, err := p.Parse(context.Background(), buf, "namespace.pptx")
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}
			if len(docs) != 1 {
				t.Fatalf("got %d documents, want one", len(docs))
			}
			const want = "First paragraph \nSecond paragraph \n"
			if docs[0].Content != want {
				t.Errorf("got slide text %q, want %q", docs[0].Content, want)
			}
		})
	}
}
