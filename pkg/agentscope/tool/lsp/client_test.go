package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type mockRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func startMockServer(t *testing.T, handlers map[string]func(id int, params json.RawMessage) any) *Server {
	t.Helper()

	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()

	go func() {
		defer serverToClientW.Close()
		reader := bufio.NewReader(clientToServerR)
		for {
			raw, err := readRawMessage(reader)
			if err != nil {
				return
			}
			var req mockRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				continue
			}
			if req.ID == 0 {
				continue
			}
			handler, ok := handlers[req.Method]
			if !ok {
				resp := Response{
					JSONRPC: "2.0",
					ID:      req.ID,
					Error:   &ResponseError{Code: -32601, Message: "method not found: " + req.Method},
				}
				data, _ := json.Marshal(resp)
				writeMessage(serverToClientW, data)
				continue
			}
			result := handler(req.ID, req.Params)
			resultData, _ := json.Marshal(result)
			resp := Response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  resultData,
			}
			data, _ := json.Marshal(resp)
			writeMessage(serverToClientW, data)
		}
	}()

	conn := NewConn(clientToServerW, serverToClientR)

	srv := &Server{
		cfg:     ServerConfig{RootDir: t.TempDir()},
		conn:    conn,
		started: true,
		opened:  make(map[string]int),
	}

	t.Cleanup(func() {
		conn.Close()
	})

	return srv
}

func TestServerDefinition(t *testing.T) {
	handlers := map[string]func(int, json.RawMessage) any{
		"textDocument/definition": func(id int, params json.RawMessage) any {
			return Location{
				URI:   "file:///test/main.go",
				Range: Range{Start: Position{Line: 10, Character: 5}},
			}
		},
	}
	srv := startMockServer(t, handlers)

	tmpFile := filepath.Join(srv.cfg.RootDir, "main.go")
	if err := os.WriteFile(tmpFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	locs, err := srv.Definition(ctx, tmpFile, 0, 0)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("expected 1 location, got %d", len(locs))
	}
	if locs[0].Range.Start.Line != 10 {
		t.Fatalf("expected line 10, got %d", locs[0].Range.Start.Line)
	}
}

func TestServerReferences(t *testing.T) {
	handlers := map[string]func(int, json.RawMessage) any{
		"textDocument/references": func(id int, params json.RawMessage) any {
			return []Location{
				{URI: "file:///test/a.go", Range: Range{Start: Position{Line: 1}}},
				{URI: "file:///test/b.go", Range: Range{Start: Position{Line: 2}}},
			}
		},
	}
	srv := startMockServer(t, handlers)

	tmpFile := filepath.Join(srv.cfg.RootDir, "main.go")
	if err := os.WriteFile(tmpFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	locs, err := srv.References(ctx, tmpFile, 0, 0)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("expected 2 locations, got %d", len(locs))
	}
}

func TestServerHover(t *testing.T) {
	handlers := map[string]func(int, json.RawMessage) any{
		"textDocument/hover": func(id int, params json.RawMessage) any {
			return Hover{
				Contents: MarkupContent{Kind: "markdown", Value: "func main()"},
			}
		},
	}
	srv := startMockServer(t, handlers)

	tmpFile := filepath.Join(srv.cfg.RootDir, "main.go")
	if err := os.WriteFile(tmpFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hover, err := srv.Hover(ctx, tmpFile, 0, 0)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if hover.Contents.Value != "func main()" {
		t.Fatalf("expected 'func main()', got %q", hover.Contents.Value)
	}
}

func TestServerDocumentSymbols(t *testing.T) {
	handlers := map[string]func(int, json.RawMessage) any{
		"textDocument/documentSymbol": func(id int, params json.RawMessage) any {
			return []DocumentSymbol{
				{Name: "main", Kind: 12, Range: Range{Start: Position{Line: 2}}},
				{Name: "MyType", Kind: 23, Range: Range{Start: Position{Line: 5}}},
			}
		},
	}
	srv := startMockServer(t, handlers)

	tmpFile := filepath.Join(srv.cfg.RootDir, "main.go")
	if err := os.WriteFile(tmpFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	symbols, err := srv.DocumentSymbols(ctx, tmpFile)
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	if len(symbols) != 2 {
		t.Fatalf("expected 2 symbols, got %d", len(symbols))
	}
	if symbols[0].Name != "main" {
		t.Fatalf("expected 'main', got %q", symbols[0].Name)
	}
}

func TestServerWorkspaceSymbol(t *testing.T) {
	handlers := map[string]func(int, json.RawMessage) any{
		"workspace/symbol": func(id int, params json.RawMessage) any {
			return []SymbolInformation{
				{
					Name: "MyFunc",
					Kind: 12,
					Location: Location{
						URI:   "file:///test/util.go",
						Range: Range{Start: Position{Line: 7}},
					},
				},
			}
		},
	}
	srv := startMockServer(t, handlers)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	symbols, err := srv.WorkspaceSymbol(ctx, "MyFunc")
	if err != nil {
		t.Fatalf("WorkspaceSymbol: %v", err)
	}
	if len(symbols) != 1 {
		t.Fatalf("expected 1 symbol, got %d", len(symbols))
	}
	if symbols[0].Name != "MyFunc" {
		t.Fatalf("expected 'MyFunc', got %q", symbols[0].Name)
	}
}

func TestFormatLocation(t *testing.T) {
	loc := Location{
		URI:   "file:///test/main.go",
		Range: Range{Start: Position{Line: 9, Character: 4}},
	}
	got := FormatLocation(loc)
	want := "/test/main.go:10:5"
	if got != want {
		t.Fatalf("FormatLocation = %q, want %q", got, want)
	}
}

func TestFormatDocumentSymbols(t *testing.T) {
	symbols := []DocumentSymbol{
		{
			Name:  "main",
			Kind:  12,
			Range: Range{Start: Position{Line: 2}},
			Children: []DocumentSymbol{
				{Name: "x", Kind: 13, Range: Range{Start: Position{Line: 3}}},
			},
		},
	}
	got := FormatDocumentSymbols(symbols)
	if !strings.Contains(got, "main (Function) line 3") {
		t.Fatalf("unexpected format: %s", got)
	}
	if !strings.Contains(got, "  x (Variable) line 4") {
		t.Fatalf("missing child symbol: %s", got)
	}
}
