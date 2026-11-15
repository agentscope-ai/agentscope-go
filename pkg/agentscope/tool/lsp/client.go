package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func (s *Server) Definition(ctx context.Context, file string, line, char int) ([]Location, error) {
	if err := s.EnsureOpen(ctx, file); err != nil {
		return nil, err
	}
	params := TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: fileURI(file)},
		Position:     Position{Line: line, Character: char},
	}
	var raw json.RawMessage
	if err := s.conn.Call(ctx, "textDocument/definition", params, &raw); err != nil {
		return nil, err
	}
	return parseLocations(raw)
}

func (s *Server) References(ctx context.Context, file string, line, char int) ([]Location, error) {
	if err := s.EnsureOpen(ctx, file); err != nil {
		return nil, err
	}
	params := ReferenceParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: fileURI(file)},
			Position:     Position{Line: line, Character: char},
		},
		Context: ReferenceContext{IncludeDeclaration: true},
	}
	var raw json.RawMessage
	if err := s.conn.Call(ctx, "textDocument/references", params, &raw); err != nil {
		return nil, err
	}
	return parseLocations(raw)
}

func (s *Server) Hover(ctx context.Context, file string, line, char int) (*Hover, error) {
	if err := s.EnsureOpen(ctx, file); err != nil {
		return nil, err
	}
	params := TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: fileURI(file)},
		Position:     Position{Line: line, Character: char},
	}
	var result Hover
	if err := s.conn.Call(ctx, "textDocument/hover", params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *Server) DocumentSymbols(ctx context.Context, file string) ([]DocumentSymbol, error) {
	if err := s.EnsureOpen(ctx, file); err != nil {
		return nil, err
	}
	params := struct {
		TextDocument TextDocumentIdentifier `json:"textDocument"`
	}{TextDocument: TextDocumentIdentifier{URI: fileURI(file)}}

	var raw json.RawMessage
	if err := s.conn.Call(ctx, "textDocument/documentSymbol", params, &raw); err != nil {
		return nil, err
	}

	var symbols []DocumentSymbol
	if err := json.Unmarshal(raw, &symbols); err == nil && len(symbols) > 0 {
		return symbols, nil
	}

	var infos []SymbolInformation
	if err := json.Unmarshal(raw, &infos); err == nil {
		for _, info := range infos {
			symbols = append(symbols, DocumentSymbol{
				Name:           info.Name,
				Kind:           info.Kind,
				Range:          info.Location.Range,
				SelectionRange: info.Location.Range,
			})
		}
		return symbols, nil
	}

	return nil, nil
}

func (s *Server) WorkspaceSymbol(ctx context.Context, query string) ([]SymbolInformation, error) {
	params := WorkspaceSymbolParams{Query: query}
	var result []SymbolInformation
	if err := s.conn.Call(ctx, "workspace/symbol", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// parseLocations handles the polymorphic response: Location | []Location | null
func parseLocations(raw json.RawMessage) ([]Location, error) {
	if raw == nil || string(raw) == "null" {
		return nil, nil
	}
	var locs []Location
	if err := json.Unmarshal(raw, &locs); err == nil {
		return locs, nil
	}
	var loc Location
	if err := json.Unmarshal(raw, &loc); err == nil {
		return []Location{loc}, nil
	}
	return nil, fmt.Errorf("unexpected definition response: %s", raw)
}

// FormatLocation formats a Location as "file:line:col" (1-based).
func FormatLocation(loc Location) string {
	path := uriToFile(loc.URI)
	return fmt.Sprintf("%s:%d:%d", path, loc.Range.Start.Line+1, loc.Range.Start.Character+1)
}

// FormatLocations formats a slice of locations, one per line.
func FormatLocations(locs []Location) string {
	if len(locs) == 0 {
		return "No results found."
	}
	var b strings.Builder
	for _, loc := range locs {
		b.WriteString(FormatLocation(loc))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// FormatHover formats hover information.
func FormatHover(h *Hover) string {
	if h == nil || h.Contents.Value == "" {
		return "No hover information available."
	}
	return h.Contents.Value
}

// FormatDocumentSymbols formats document symbols as an indented tree.
func FormatDocumentSymbols(symbols []DocumentSymbol) string {
	if len(symbols) == 0 {
		return "No symbols found."
	}
	var b strings.Builder
	formatSymbolTree(&b, symbols, 0)
	return strings.TrimRight(b.String(), "\n")
}

func formatSymbolTree(b *strings.Builder, symbols []DocumentSymbol, indent int) {
	prefix := strings.Repeat("  ", indent)
	for i := range symbols {
		kind := SymbolKindName(symbols[i].Kind)
		fmt.Fprintf(b, "%s%s (%s) line %d\n", prefix, symbols[i].Name, kind, symbols[i].Range.Start.Line+1)
		if len(symbols[i].Children) > 0 {
			formatSymbolTree(b, symbols[i].Children, indent+1)
		}
	}
}

// FormatWorkspaceSymbols formats workspace symbol results.
func FormatWorkspaceSymbols(symbols []SymbolInformation) string {
	if len(symbols) == 0 {
		return "No symbols found."
	}
	var b strings.Builder
	for _, s := range symbols {
		kind := SymbolKindName(s.Kind)
		path := uriToFile(s.Location.URI)
		fmt.Fprintf(&b, "%s (%s) at %s:%d\n", s.Name, kind, path, s.Location.Range.Start.Line+1)
	}
	return strings.TrimRight(b.String(), "\n")
}
