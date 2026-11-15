package tool

import (
	"context"
	"fmt"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGrepRequestsPathOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprev=''\nfor arg in \"$@\"; do\n if [ \"$prev\" = '--sort' ] && [ \"$arg\" = 'path' ]; then printf 'a\\nb\\nc\\n'; exit 0; fi\n prev=\"$arg\"\ndone\nprintf 'c\\na\\nb\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "rg"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, mode := range []string{"content", "count", "files_with_matches"} {
		r, err := (&grepTool{}).tryRipgrep(context.Background(), &grepOptions{pattern: "x", searchPath: dir, outputMode: mode, page: 2, pageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Content[0].(message.TextBlock).Text; !strings.HasPrefix(got, "b") {
			t.Fatalf("%s page 2=%q", mode, got)
		}
	}
}

func TestGrepPaginationStableAcrossPages(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"c.txt", "a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("match\n", 6)), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, native := range []bool{false, true} {
		if native {
			if _, err := exec.LookPath("rg"); err != nil {
				continue
			}
		}
		for _, mode := range []string{"content", "count", "files_with_matches"} {
			for page := 1; page <= 3; page++ {
				opts := &grepOptions{pattern: "match", searchPath: dir, outputMode: mode, page: page, pageSize: 2, maxCount: 2}
				var r *ToolResponse
				var err error
				if native {
					r, err = (&grepTool{}).tryRipgrep(context.Background(), opts)
				} else {
					r, err = (&grepTool{}).goGrep(opts)
				}
				if err != nil {
					t.Fatal(err)
				}
				got := r.Content[0].(message.TextBlock).Text
				if mode == "content" {
					want := filepath.Join(dir, fmt.Sprintf("%c.txt", 'a'+page-1))
					if !strings.HasPrefix(got, want+":1:match\n"+want+":2:match") {
						t.Fatalf("native=%t page=%d got=%q", native, page, got)
					}
				}
				if mode != "content" && page == 3 && !strings.Contains(got, "No results") {
					t.Fatalf("expected boundary page: %q", got)
				}
			}
		}
	}
}
