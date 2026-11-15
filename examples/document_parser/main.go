package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/rag/parser"
)

// This example demonstrates the document parser system: parsing text into
// chunked documents, using ChunkText directly, and listing supported
// extensions for each parser type.

func main() {
	ctx := context.Background()

	// Sample multi-paragraph text for parsing.
	sampleText := `Artificial intelligence has transformed the way we build software.
Modern AI systems leverage large language models to understand and generate
natural language, enabling applications from code completion to autonomous agents.

Multi-agent frameworks like AgentScope allow developers to orchestrate
multiple AI agents that collaborate to solve complex tasks. Each agent
can have specialized capabilities, tools, and knowledge bases.

Retrieval-Augmented Generation (RAG) combines the power of large language
models with external knowledge retrieval. Documents are parsed, chunked,
embedded, and indexed so that relevant context can be fetched at query time.`

	// Create a TextParser with small chunks for demonstration.
	cfg := parser.ChunkConfig{
		MaxChunkSize: 120,
		Overlap:      20,
	}
	tp := parser.NewTextParser(cfg)

	// Parse the sample text.
	fmt.Println("=== Parse Multi-Paragraph Text ===")
	fmt.Printf("  Chunk config: MaxChunkSize=%d, Overlap=%d\n", cfg.MaxChunkSize, cfg.Overlap)
	fmt.Println()

	docs, err := tp.Parse(ctx, strings.NewReader(sampleText), "sample.txt")
	if err != nil {
		fmt.Println("parse error:", err)
		return
	}

	for _, doc := range docs {
		chunkIdx := doc.Meta["chunk_index"]
		preview := doc.Content
		if len(preview) > 80 {
			preview = preview[:80] + "..."
		}
		// Replace newlines for cleaner display.
		preview = strings.ReplaceAll(preview, "\n", " ")
		fmt.Printf("  [%s] chunk=%v len=%d\n    %q\n",
			doc.ID, chunkIdx, len(doc.Content), preview)
	}

	// Demonstrate ChunkText function directly.
	fmt.Println("\n=== ChunkText Direct Usage ===")
	shortText := "The quick brown fox jumps over the lazy dog. Pack my box with five dozen liquor jugs."
	smallCfg := parser.ChunkConfig{MaxChunkSize: 40, Overlap: 10}
	chunks := parser.ChunkText(shortText, smallCfg)
	fmt.Printf("  Input length: %d chars\n", len(shortText))
	fmt.Printf("  Config: MaxChunkSize=%d, Overlap=%d\n", smallCfg.MaxChunkSize, smallCfg.Overlap)
	fmt.Printf("  Produced %d chunks:\n", len(chunks))
	for i, chunk := range chunks {
		fmt.Printf("    [%d] %q\n", i, chunk)
	}

	// Show supported extensions for each parser type.
	fmt.Println("\n=== Supported Extensions by Parser ===")
	defaultCfg := parser.DefaultChunkConfig()
	parsers := []struct {
		name string
		p    parser.Parser
	}{
		{"TextParser", parser.NewTextParser(defaultCfg)},
		{"WordParser", parser.NewWordParser(defaultCfg)},
		{"ExcelParser", parser.NewExcelParser(defaultCfg)},
		{"PPTParser", parser.NewPPTParser(defaultCfg)},
		{"PDFParser", parser.NewPDFParser(defaultCfg)},
	}
	for _, entry := range parsers {
		exts := entry.p.SupportedExtensions()
		fmt.Printf("  %-12s -> %s\n", entry.name, strings.Join(exts, ", "))
	}

	fmt.Println("\n=== Done ===")
}
