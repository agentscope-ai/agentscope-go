package tool

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// firstDataBlock finds the image payload in a tool response. The response also
// carries a leading TextBlock placeholder so text-only consumers do not see an
// empty result.
func firstDataBlock(t *testing.T, resp *ToolResponse) message.DataBlock {
	t.Helper()
	for _, b := range resp.Content {
		if db, ok := b.(message.DataBlock); ok {
			return db
		}
	}
	t.Fatalf("no DataBlock in response content: %#v", resp.Content)
	return message.DataBlock{}
}

// requirePlaceholder asserts the response leads with an honest text
// description of the image. Without it, pipelines that only read TextBlocks
// record an empty tool result and the model concludes the file is empty.
func requirePlaceholder(t *testing.T, resp *ToolResponse, name string) {
	t.Helper()
	if len(resp.Content) == 0 {
		t.Fatal("empty response content")
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("content[0] = %T, want TextBlock placeholder", resp.Content[0])
	}
	if !strings.Contains(tb.Text, name) {
		t.Errorf("placeholder %q should mention the file name", tb.Text)
	}
	if !strings.Contains(tb.Text, "bytes") {
		t.Errorf("placeholder %q should mention the size", tb.Text)
	}
}

// Upstream #2114: image files return as DataBlocks so multimodal models can
// see them instead of receiving mojibake text.
func TestReadToolImageReturnsDataBlock(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "pixel.png")
	payload := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3}
	if err := os.WriteFile(png, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	rt := &readTool{}
	resp, err := rt.Execute(context.Background(), map[string]any{"file_path": png})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %v", resp.State)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2 (placeholder + image)", len(resp.Content))
	}
	requirePlaceholder(t, resp, "pixel.png")
	db := firstDataBlock(t, resp)
	if db.GetMediaType() != "image/png" {
		t.Errorf("media type = %q", db.GetMediaType())
	}
	src, ok := db.Source.(message.Base64Source)
	if !ok {
		t.Fatalf("source = %T, want Base64Source", db.Source)
	}
	decoded, err := base64.StdEncoding.DecodeString(src.Data)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Error("decoded image bytes differ from the file")
	}
	if db.Name != "pixel.png" {
		t.Errorf("name = %q", db.Name)
	}
}

func TestReadToolBackendImageReturnsDataBlock(t *testing.T) {
	b := &stubStatterBackend{files: map[string][]byte{"img.jpg": {0xff, 0xd8, 0xff}}}
	ctx := WithBackend(context.Background(), b)
	rt := &readTool{}
	resp, err := rt.Execute(ctx, map[string]any{"file_path": "img.jpg"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	requirePlaceholder(t, resp, "img.jpg")
	db := firstDataBlock(t, resp)
	if db.GetMediaType() != "image/jpeg" {
		t.Errorf("media type = %q", db.GetMediaType())
	}
}

func TestReadToolTextUnaffected(t *testing.T) {
	dir := t.TempDir()
	txt := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(txt, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := &readTool{}
	resp, err := rt.Execute(context.Background(), map[string]any{"file_path": txt})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, ok := resp.Content[0].(message.TextBlock); !ok {
		t.Errorf("text file content[0] = %T, want TextBlock", resp.Content[0])
	}
}

// A large image must not be inlined: the token estimator decodes base64 back to
// raw bytes and divides by four, so an inlined image costs roughly fileSize/4
// tokens, and one screenshot could exhaust the context window and force an
// immediate compression. It is reported as text instead.
func TestReadToolOversizeImageFallsBackToText(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "huge.png")
	if err := os.WriteFile(big, make([]byte, MaxInlineImageBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := &readTool{}
	resp, err := rt.Execute(context.Background(), map[string]any{"file_path": big})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %v, want success (the read did not fail)", resp.State)
	}
	for _, b := range resp.Content {
		if _, isData := b.(message.DataBlock); isData {
			t.Fatal("oversize image must not be inlined as a DataBlock")
		}
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("content[0] = %T, want TextBlock", resp.Content[0])
	}
	if !strings.Contains(tb.Text, "too large to inline") {
		t.Errorf("text %q should explain why the image was not loaded", tb.Text)
	}
}

// The same cap applies on the backend path, where the size check happens after
// the bytes are already fetched.
func TestReadToolBackendOversizeImageFallsBackToText(t *testing.T) {
	b := &stubStatterBackend{files: map[string][]byte{"big.png": make([]byte, MaxInlineImageBytes+1)}}
	ctx := WithBackend(context.Background(), b)

	rt := &readTool{}
	resp, err := rt.Execute(ctx, map[string]any{"file_path": "big.png"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, blk := range resp.Content {
		if _, isData := blk.(message.DataBlock); isData {
			t.Fatal("oversize backend image must not be inlined")
		}
	}
}
