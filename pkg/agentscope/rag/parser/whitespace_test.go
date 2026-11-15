package parser

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestChunkTextDropsOnlyBlankWindows(t *testing.T) {
	for _, unit := range []ChunkUnit{ChunkUnitChars, ChunkUnitApproxTokens} {
		cfg := ChunkConfig{MaxChunkSize: 4, Unit: unit}
		if unit == ChunkUnitApproxTokens {
			cfg.MaxChunkSize = 1
		}
		for _, text := range []string{"", " \t\n\u2003", strings.Repeat(" ", 20)} {
			if got := ChunkText(text, cfg); len(got) != 0 {
				t.Fatalf("%s blank: %q", unit, got)
			}
		}
		text := " a      b  "
		if got := ChunkText(text, cfg); !reflect.DeepEqual(got, []string{" a  ", "b  "}) {
			t.Fatalf("%s chunks=%q", unit, got)
		}
		docs, err := NewTextParser(cfg).Parse(context.Background(), strings.NewReader(text), "x.txt")
		if err != nil {
			t.Fatal(err)
		}
		for i, d := range docs {
			if d.Meta["chunk_index"] != i {
				t.Fatalf("noncontiguous index: %+v", d)
			}
		}
	}
}
