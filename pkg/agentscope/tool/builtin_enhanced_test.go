package tool

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// --- Bash tool tests ---

func TestBashTool_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	bt := BashTool()
	if bt.Name() != "Bash" {
		t.Fatalf("name = %q, want %q", bt.Name(), "Bash")
	}

	resp, err := bt.Execute(context.Background(), map[string]any{"command": "echo hello"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %s, want success", resp.State)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "hello") {
		t.Fatalf("output missing 'hello': %q", text)
	}
}

func TestBashTool_EmptyCommand(t *testing.T) {
	bt := BashTool()
	resp, err := bt.Execute(context.Background(), map[string]any{"command": "  "})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultError {
		t.Fatal("expected error for empty command")
	}
}

func TestBashTool_MissingCommand(t *testing.T) {
	bt := BashTool()
	resp, err := bt.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultError {
		t.Fatal("expected error for missing command")
	}
}

func TestBashTool_NonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	bt := BashTool()
	resp, err := bt.Execute(context.Background(), map[string]any{"command": "exit 42"})
	if err != nil {
		t.Fatal(err)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "42") {
		t.Fatalf("output should contain exit code 42: %q", text)
	}
}

// --- Read tool tests ---

func TestReadTool_FullFile(t *testing.T) {
	tmp := createTempFile(t, "line1\nline2\nline3\n")
	defer os.Remove(tmp)

	rt := ReadTool()
	resp, err := rt.Execute(context.Background(), map[string]any{"file_path": tmp})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %s", resp.State)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "1\tline1") {
		t.Fatalf("expected line numbers, got: %q", text)
	}
	if !strings.Contains(text, "3\tline3") {
		t.Fatalf("expected line3, got: %q", text)
	}
}

func TestReadTool_OffsetAndLimit(t *testing.T) {
	tmp := createTempFile(t, "a\nb\nc\nd\ne\n")
	defer os.Remove(tmp)

	rt := ReadTool()
	resp, err := rt.Execute(context.Background(), map[string]any{
		"file_path": tmp,
		"offset":    3.0, // 1-based: line 3 = "c"
		"limit":     2.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := getResponseText(resp)
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), text)
	}
	if !strings.Contains(lines[0], "c") {
		t.Fatalf("first line should be 'c', got %q", lines[0])
	}
}

func TestReadTool_FileNotFound(t *testing.T) {
	rt := ReadTool()
	resp, err := rt.Execute(context.Background(), map[string]any{"file_path": "/tmp/nonexistent_abc123.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultError {
		t.Fatal("expected error for missing file")
	}
}

func TestReadTool_Directory(t *testing.T) {
	rt := ReadTool()
	resp, err := rt.Execute(context.Background(), map[string]any{"file_path": os.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultError {
		t.Fatal("expected error for directory path")
	}
}

// --- Write tool tests ---

func TestWriteTool_CreateNew(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "new.txt")

	wt := WriteTool()
	resp, err := wt.Execute(context.Background(), map[string]any{
		"file_path": path,
		"content":   "hello world",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %s, text = %q", resp.State, getResponseText(resp))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Fatalf("file content = %q", data)
	}
}

func TestWriteTool_Overwrite(t *testing.T) {
	tmp := createTempFile(t, "original")
	defer os.Remove(tmp)

	wt := WriteTool()
	_, err := wt.Execute(context.Background(), map[string]any{
		"file_path": tmp,
		"content":   "replaced",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "replaced" {
		t.Fatalf("content = %q, want 'replaced'", data)
	}
}

// --- Edit tool tests ---

func TestEditTool_SingleReplace(t *testing.T) {
	tmp := createTempFile(t, "hello world\nfoo bar\n")
	defer os.Remove(tmp)

	et := EditTool()
	resp, err := et.Execute(context.Background(), map[string]any{
		"file_path":  tmp,
		"old_string": "foo bar",
		"new_string": "baz qux",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %s, text = %q", resp.State, getResponseText(resp))
	}

	data, _ := os.ReadFile(tmp)
	if !strings.Contains(string(data), "baz qux") {
		t.Fatalf("file not updated: %q", data)
	}
}

func TestEditTool_ReplaceAll(t *testing.T) {
	tmp := createTempFile(t, "aaa bbb aaa ccc aaa\n")
	defer os.Remove(tmp)

	et := EditTool()
	resp, err := et.Execute(context.Background(), map[string]any{
		"file_path":   tmp,
		"old_string":  "aaa",
		"new_string":  "XXX",
		"replace_all": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %s", resp.State)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "3") {
		t.Fatalf("should report 3 replacements: %q", text)
	}

	data, _ := os.ReadFile(tmp)
	if strings.Contains(string(data), "aaa") {
		t.Fatalf("still contains 'aaa': %q", data)
	}
}

func TestEditTool_NotUnique(t *testing.T) {
	tmp := createTempFile(t, "foo\nfoo\n")
	defer os.Remove(tmp)

	et := EditTool()
	resp, err := et.Execute(context.Background(), map[string]any{
		"file_path":  tmp,
		"old_string": "foo",
		"new_string": "bar",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultError {
		t.Fatal("expected error for non-unique match")
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "not unique") {
		t.Fatalf("error should mention 'not unique': %q", text)
	}
}

func TestEditTool_NotFound(t *testing.T) {
	tmp := createTempFile(t, "hello\n")
	defer os.Remove(tmp)

	et := EditTool()
	resp, err := et.Execute(context.Background(), map[string]any{
		"file_path":  tmp,
		"old_string": "xyz",
		"new_string": "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultError {
		t.Fatal("expected error when old_string not found")
	}
}

// --- Glob tool tests ---

func TestGlobTool_Match(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte(""), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.go"), []byte(""), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "c.txt"), []byte(""), 0o644)

	gt := GlobTool()
	resp, err := gt.Execute(context.Background(), map[string]any{
		"pattern":  "*.go",
		"base_dir": dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "a.go") || !strings.Contains(text, "b.go") {
		t.Fatalf("should match .go files: %q", text)
	}
	if strings.Contains(text, "c.txt") {
		t.Fatalf("should not match .txt: %q", text)
	}
}

func TestGlobTool_DoublestarMatch(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	_ = os.MkdirAll(sub, 0o755)
	_ = os.WriteFile(filepath.Join(sub, "deep.go"), []byte(""), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "top.go"), []byte(""), 0o644)

	gt := GlobTool()
	resp, err := gt.Execute(context.Background(), map[string]any{
		"pattern":  "**/*.go",
		"base_dir": dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "deep.go") {
		t.Fatalf("should match deep.go: %q", text)
	}
	if !strings.Contains(text, "top.go") {
		t.Fatalf("should match top.go: %q", text)
	}
}

func TestGlobTool_NoMatch(t *testing.T) {
	dir := t.TempDir()
	gt := GlobTool()
	resp, err := gt.Execute(context.Background(), map[string]any{
		"pattern":  "*.xyz",
		"base_dir": dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "No files matched") {
		t.Fatalf("expected no match message: %q", text)
	}
}

// --- Grep tool tests ---

func TestGrepTool_Found(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "test.go"), []byte("func main() {\n\tfmt.Println(\"hello\")\n}\n"), 0o644)

	gt := GrepTool()
	resp, err := gt.Execute(context.Background(), map[string]any{
		"pattern": "Println",
		"path":    dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "Println") {
		t.Fatalf("should find Println: %q", text)
	}
	if !strings.Contains(text, "test.go") {
		t.Fatalf("should include filename: %q", text)
	}
}

func TestGrepTool_NotFound(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello world\n"), 0o644)

	gt := GrepTool()
	resp, err := gt.Execute(context.Background(), map[string]any{
		"pattern": "zzzzz_nonexistent",
		"path":    dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "No matches") {
		t.Fatalf("expected no matches: %q", text)
	}
}

func TestGrepTool_WithInclude(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("target line\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("target line\n"), 0o644)

	gt := GrepTool()
	resp, err := gt.Execute(context.Background(), map[string]any{
		"pattern": "target",
		"path":    dir,
		"include": "*.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "a.go") {
		t.Fatalf("should find in a.go: %q", text)
	}
	if strings.Contains(text, "b.txt") {
		t.Fatalf("should not match b.txt: %q", text)
	}
}

// --- NewEnhancedToolkit test ---

func TestNewEnhancedToolkit(t *testing.T) {
	tk := NewEnhancedToolkit()
	expected := []string{"Bash", "Read", "Write", "Edit", "MultiEdit", "ApplyPatch", "Glob", "Grep"}
	for _, name := range expected {
		if tk.Get(name) == nil {
			t.Errorf("tool %q not found in enhanced toolkit", name)
		}
	}
	schemas := tk.GetToolSchemas()
	if len(schemas) != len(expected) {
		t.Fatalf("expected %d schemas, got %d", len(expected), len(schemas))
	}
}

func TestNewEnhancedToolkit_CaseInsensitiveFallback(t *testing.T) {
	tk := NewEnhancedToolkit()
	// Old lowercase names should still resolve via case-insensitive fallback
	oldNames := []string{"bash", "read", "write", "edit", "glob", "grep"}
	for _, name := range oldNames {
		if tk.Get(name) == nil {
			t.Errorf("tool %q not found via case-insensitive fallback", name)
		}
	}
}

// --- helpers ---

func createTempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "agentscope-test-*")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(content)
	f.Close()
	return f.Name()
}

// --- Read cache integration tests ---

func TestReadTool_CacheHit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cached.txt")
	_ = os.WriteFile(path, []byte("line1\nline2\nline3"), 0o644)

	rc := NewReadCache(10, 100*1024)
	ctx := WithReadCache(context.Background(), rc)

	rt := ReadTool()

	// First read: cache miss, populates cache
	resp, err := rt.Execute(ctx, map[string]any{"file_path": path})
	if err != nil {
		t.Fatal(err)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "line1") {
		t.Fatalf("expected content, got: %s", text)
	}
	if rc.Len() != 1 {
		t.Fatalf("cache should have 1 entry, got %d", rc.Len())
	}

	// Second read: cache hit
	resp2, err := rt.Execute(ctx, map[string]any{"file_path": path})
	if err != nil {
		t.Fatal(err)
	}
	text2 := getResponseText(resp2)
	if text != text2 {
		t.Error("cache hit should return same content")
	}
}

func TestReadTool_NoCacheContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nocache.txt")
	_ = os.WriteFile(path, []byte("content"), 0o644)

	rt := ReadTool()
	resp, err := rt.Execute(context.Background(), map[string]any{"file_path": path})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatal("should succeed without cache")
	}
}

func TestEditTool_ReadBeforeWriteGuard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "guard.txt")
	_ = os.WriteFile(path, []byte("old text"), 0o644)

	rc := NewReadCache(10, 100*1024)
	ctx := WithReadCache(context.Background(), rc)

	et := EditTool()

	// Try editing without reading first
	resp, err := et.Execute(ctx, map[string]any{
		"file_path":  path,
		"old_string": "old",
		"new_string": "new",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultError {
		t.Fatal("should fail without reading first")
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "must read the file first") {
		t.Fatalf("unexpected error: %s", text)
	}

	// Read the file first
	rt := ReadTool()
	_, _ = rt.Execute(ctx, map[string]any{"file_path": path})

	// Now edit should work
	resp2, err := et.Execute(ctx, map[string]any{
		"file_path":  path,
		"old_string": "old text",
		"new_string": "new text",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp2.State != message.ToolResultSuccess {
		t.Fatalf("should succeed after reading, got: %s", getResponseText(resp2))
	}
}

func TestWriteTool_ReadBeforeWriteGuard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "writeguard.txt")
	_ = os.WriteFile(path, []byte("existing"), 0o644)

	rc := NewReadCache(10, 100*1024)
	ctx := WithReadCache(context.Background(), rc)

	wt := WriteTool()

	// Try overwriting without reading first
	resp, err := wt.Execute(ctx, map[string]any{
		"file_path": path,
		"content":   "new content",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultError {
		t.Fatal("should fail when overwriting without reading first")
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "must read the file first") {
		t.Fatalf("unexpected error: %s", text)
	}

	// New file should be allowed without reading
	newPath := filepath.Join(dir, "brand_new.txt")
	resp2, err := wt.Execute(ctx, map[string]any{
		"file_path": newPath,
		"content":   "fresh content",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp2.State != message.ToolResultSuccess {
		t.Fatalf("new file should be allowed: %s", getResponseText(resp2))
	}
}

func TestEditTool_NoGuardWithoutCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "noguard.txt")
	_ = os.WriteFile(path, []byte("old text"), 0o644)

	et := EditTool()

	// Without ReadCache in context, edit should work normally
	resp, err := et.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "old text",
		"new_string": "new text",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("should succeed without cache context: %s", getResponseText(resp))
	}
}

// --- Bash streaming tests ---

func TestBashTool_StreamingOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	bt := BashTool()
	st, ok := bt.(StreamingTool)
	if !ok {
		t.Fatal("BashTool should implement StreamingTool")
	}

	ch, err := st.ExecuteStream(context.Background(), map[string]any{
		"command": `echo line1; echo line2; echo line3`,
	})
	if err != nil {
		t.Fatal(err)
	}

	var chunks []ToolChunk
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks (deltas + final), got %d", len(chunks))
	}

	// Last chunk should be final
	last := chunks[len(chunks)-1]
	if !last.IsFinal {
		t.Fatal("last chunk should be final")
	}
	if last.State != message.ToolResultSuccess {
		t.Fatalf("final state = %s, want success", last.State)
	}

	// Non-final chunks should contain streaming lines
	var streamedLines int
	for _, chunk := range chunks {
		if !chunk.IsFinal {
			streamedLines++
		}
	}
	if streamedLines < 3 {
		t.Fatalf("expected at least 3 streamed lines, got %d", streamedLines)
	}
}

func TestBashTool_StreamingNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	bt := BashTool()
	st := bt.(StreamingTool)

	ch, err := st.ExecuteStream(context.Background(), map[string]any{
		"command": "echo fail; exit 1",
	})
	if err != nil {
		t.Fatal(err)
	}

	var final ToolChunk
	for chunk := range ch {
		if chunk.IsFinal {
			final = chunk
		}
	}

	if final.State != message.ToolResultError {
		t.Fatalf("final state = %s, want error for non-zero exit", final.State)
	}
}

func TestBashTool_SynchronousNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	resp, err := BashTool().Execute(context.Background(), map[string]any{
		"command": "echo fail; exit 7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("state = %s, want error for non-zero exit", resp.State)
	}
	if text := getResponseText(resp); !strings.Contains(text, `"exit_code":7`) {
		t.Fatalf("response should preserve the exit code, got %q", text)
	}
}

func TestBashTool_CollectStream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	bt := BashTool()
	st := bt.(StreamingTool)

	ch, err := st.ExecuteStream(context.Background(), map[string]any{
		"command": "echo hello; echo world",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := CollectStream(ch)
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %s, want success", resp.State)
	}
	text := getResponseText(resp)
	if !strings.Contains(text, "hello") {
		t.Fatalf("collected output should contain 'hello': %q", text)
	}
}

func TestBashTool_Description(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	bt := BashTool()
	resp, err := bt.Execute(context.Background(), map[string]any{
		"command":     "echo test",
		"description": "Running a test command",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatal("should succeed with description param")
	}
}

func TestBashTool_ContextCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	bt := BashTool()
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after a brief delay so the command starts
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	resp, err := bt.Execute(ctx, map[string]any{
		"command": "sleep 10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultInterrupted {
		t.Fatalf("state = %s, want interrupted", resp.State)
	}
}
