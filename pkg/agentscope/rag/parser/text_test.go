package parser

import (
	"context"
	"strings"
	"testing"
)

func TestTextParser_SimpleContent(t *testing.T) {
	p := NewTextParser(ChunkConfig{MaxChunkSize: 1000, Overlap: 100})
	content := "Hello, this is a test document."
	docs, err := p.Parse(context.Background(), strings.NewReader(content), "test.txt")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 document, got %d", len(docs))
	}
	doc := docs[0]
	if doc.ID != "test.txt_chunk_0" {
		t.Errorf("unexpected ID: %q", doc.ID)
	}
	if doc.Content != content {
		t.Errorf("unexpected Content: %q", doc.Content)
	}
	if doc.Meta["filename"] != "test.txt" {
		t.Errorf("unexpected filename in meta: %v", doc.Meta["filename"])
	}
	if doc.Meta["chunk_index"] != 0 {
		t.Errorf("unexpected chunk_index in meta: %v", doc.Meta["chunk_index"])
	}
}

func TestTextParserLeadingBOM(t *testing.T) {
	p := NewTextParser(ChunkConfig{MaxChunkSize: 3})
	docs, err := p.Parse(context.Background(), strings.NewReader("\uFEFFA\uFEFFB"), "bom.txt")
	if err != nil || len(docs) != 1 || docs[0].Content != "A\uFEFFB" {
		t.Fatalf("docs=%v err=%v; want one chunk with its interior BOM intact", docs, err)
	}
	docs, err = p.Parse(context.Background(), strings.NewReader("\uFEFF"), "empty.txt")
	if err != nil || docs != nil {
		t.Fatalf("BOM-only file: docs=%v err=%v, want nil", docs, err)
	}
}

func TestTextParser_EmptyContent(t *testing.T) {
	p := NewTextParser(ChunkConfig{MaxChunkSize: 1000, Overlap: 100})
	docs, err := p.Parse(context.Background(), strings.NewReader(""), "empty.txt")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if docs != nil {
		t.Fatalf("expected nil for empty content, got %d docs", len(docs))
	}
}

func TestTextParser_MultipleChunks(t *testing.T) {
	p := NewTextParser(ChunkConfig{MaxChunkSize: 10, Overlap: 3})
	content := "This is a longer text that should produce multiple chunks."
	docs, err := p.Parse(context.Background(), strings.NewReader(content), "multi.txt")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(docs) < 2 {
		t.Fatalf("expected multiple documents, got %d", len(docs))
	}
	for i, doc := range docs {
		if doc.Meta["chunk_index"] != i {
			t.Errorf("doc %d: expected chunk_index %d, got %v", i, i, doc.Meta["chunk_index"])
		}
	}
}
