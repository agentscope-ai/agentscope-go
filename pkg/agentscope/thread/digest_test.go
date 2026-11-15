package thread

import (
	"reflect"
	"strings"
	"testing"
)

func TestDigestBuilderEmpty(t *testing.T) {
	var b DigestBuilder
	if d := b.Digest(); d != nil {
		t.Fatalf("empty builder gave %+v, want nil", d)
	}
	b.Add("  ", `{"path":"a"}`)
	if d := b.Digest(); d != nil {
		t.Fatalf("a nameless call gave %+v, want nil", d)
	}
}

// TestDigestBuilderListsEveryCall: one item per call in call order, with
// the raw arguments cut at DigestArgsRunes (stage 0's tool summary).
func TestDigestBuilderListsEveryCall(t *testing.T) {
	var b DigestBuilder
	b.Add("read_file", `{"path":"a.go"}`)
	b.Add("read_file", `{"path":"a.go"}`)
	b.Add("write_file", `{"path":"b.go","content":"`+strings.Repeat("x", 300)+`"}`)
	b.Add("list", "")
	got := b.Digest()
	long := `{"path":"b.go","content":"` + strings.Repeat("x", 300) + `"}`
	want := &ToolDigest{Calls: []ToolCallDigest{
		{Name: "read_file", Args: `{"path":"a.go"}`},
		{Name: "read_file", Args: `{"path":"a.go"}`},
		{Name: "write_file", Args: string([]rune(long)[:DigestArgsRunes]) + "..."},
		{Name: "list"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("digest\n got %+v\nwant %+v", got, want)
	}
	lines := strings.Split(got.Render(), "\n")
	if len(lines) != 4 || lines[0] != `read_file {"path":"a.go"}` || lines[3] != "list" ||
		!strings.HasPrefix(lines[2], `write_file {"path":"b.go"`) || !strings.HasSuffix(lines[2], "...") {
		t.Errorf("render %q", got.Render())
	}
	// A digest is a snapshot; later calls do not change it.
	b.Add("read_file", `{"path":"z.go"}`)
	if len(got.Calls) != 4 {
		t.Errorf("snapshot changed: %+v", got.Calls)
	}
}

func TestToolTarget(t *testing.T) {
	long := strings.Repeat("é", 100)
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"nil args", nil, ""},
		{"path first", map[string]any{"query": "q", "path": "p"}, "p"},
		{"order", map[string]any{"query": "q", "directory": "d", "dir": "dd", "uri": "u"}, "u"},
		{"file_path", map[string]any{"file_path": "f"}, "f"},
		{"file", map[string]any{"file": "f"}, "f"},
		{"filename", map[string]any{"filename": "n"}, "n"},
		{"url", map[string]any{"url": "https://x"}, "https://x"},
		{"dir", map[string]any{"dir": "d"}, "d"},
		{"directory", map[string]any{"directory": "d"}, "d"},
		{"pattern", map[string]any{"pattern": "*.go"}, "*.go"},
		{"query", map[string]any{"query": "weather"}, "weather"},
		{"empty skipped", map[string]any{"path": "  ", "file": "f"}, "f"},
		{"non-string skipped", map[string]any{"path": 3, "url": "u"}, "u"},
		{"command first word", map[string]any{"command": "  git status --short"}, "git"},
		{"cmd", map[string]any{"cmd": "ls -la"}, "ls"},
		{"blank command", map[string]any{"command": "   ", "cmd": "pwd"}, "pwd"},
		{"named argument wins over command", map[string]any{"command": "cat x", "path": "y"}, "y"},
		{"whitespace folded", map[string]any{"query": "a\nb\t c"}, "a b c"},
		{"truncated", map[string]any{"path": long}, strings.Repeat("é", 79) + "…"},
		{"no target", map[string]any{"limit": 5}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToolTarget(tc.args); got != tc.want {
				t.Errorf("target %q, want %q", got, tc.want)
			}
		})
	}
}
