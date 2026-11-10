package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// sharedAgent is one agent's view of a shared workspace: its own read cache.
type sharedAgent struct {
	t   *testing.T
	ctx context.Context
	rc  *ReadCache
}

func newSharedAgent(t *testing.T, base context.Context, reject bool) *sharedAgent {
	rc := NewReadCache(0, 0)
	rc.SetRejectStaleWrites(reject)
	return &sharedAgent{t: t, ctx: WithReadCache(base, rc), rc: rc}
}

func (a *sharedAgent) run(tl Tool, args map[string]any) *ToolResponse {
	a.t.Helper()
	resp, err := tl.Execute(a.ctx, args)
	if err != nil {
		a.t.Fatalf("%s: %v", tl.Name(), err)
	}
	return resp
}

func (a *sharedAgent) read(path string) string {
	a.t.Helper()
	resp := a.run(ReadTool(), map[string]any{"file_path": path})
	if resp.State != message.ToolResultSuccess {
		a.t.Fatalf("read %s: %s", path, respText(resp))
	}
	return respText(resp)
}

func (a *sharedAgent) write(path, content string) *ToolResponse {
	a.t.Helper()
	return a.run(WriteTool(), map[string]any{"file_path": path, "content": content})
}

func (a *sharedAgent) edit(path, from, to string) *ToolResponse {
	a.t.Helper()
	return a.run(EditTool(), map[string]any{"file_path": path, "old_string": from, "new_string": to})
}

func respText(r *ToolResponse) string {
	var sb strings.Builder
	for _, b := range r.Content {
		if tb, ok := b.(message.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

func isStale(r *ToolResponse) bool {
	return r.State == message.ToolResultError && strings.Contains(respText(r), "changed since you last read it")
}

func mustContent(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestStaleWriteIsRefusedOnASharedWorkspace(t *testing.T) {
	f := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(f, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newSharedAgent(t, context.Background(), true)
	b := newSharedAgent(t, context.Background(), true)
	a.read(f)
	b.read(f)
	if r := b.write(f, "package main\n\nfunc b() {}\n"); r.State != message.ToolResultSuccess {
		t.Fatalf("b's write: %s", respText(r))
	}

	if r := a.write(f, "package main\n\nfunc a() {}\n"); !isStale(r) {
		t.Fatalf("a's write from the old copy = %v %q, want the stale refusal", r.State, respText(r))
	}
	if got := mustContent(t, f); !strings.Contains(got, "func b()") {
		t.Fatalf("the refused write still changed the file: %q", got)
	}
	// Rewriting exactly what is there loses nothing.
	if r := a.write(f, "package main\n\nfunc b() {}\n"); r.State != message.ToolResultSuccess {
		t.Fatalf("a no-op write was refused: %s", respText(r))
	}
	if !strings.Contains(a.read(f), "func b()") {
		t.Fatal("a's read must return b's version")
	}
	if r := a.write(f, "package main\n\nfunc b() {}\n\nfunc a() {}\n"); r.State != message.ToolResultSuccess {
		t.Fatalf("a's write after reading again: %s", respText(r))
	}
	// A new file needs no read.
	if r := a.write(filepath.Join(filepath.Dir(f), "new.go"), "package main\n"); r.State != message.ToolResultSuccess {
		t.Fatalf("creating a file: %s", respText(r))
	}
}

// TestStaleWriteOffKeepsTheOldBehavior shows what the check prevents: by
// default the write from the old copy replaces the other agent's work.
func TestStaleWriteOffKeepsTheOldBehavior(t *testing.T) {
	f := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newSharedAgent(t, context.Background(), false)
	b := newSharedAgent(t, context.Background(), false)
	a.read(f)
	b.read(f)
	b.write(f, "v2 from b\n")
	if r := a.write(f, "v2 from a\n"); r.State != message.ToolResultSuccess {
		t.Fatalf("with the check off the write must go through: %s", respText(r))
	}
	if got := mustContent(t, f); got != "v2 from a\n" {
		t.Fatalf("content = %q", got)
	}
}

// TestStaleWriteSameModificationTime covers a change that keeps the file's
// modification time: the version is its content, not its mtime, and Read
// does not serve the stale cached copy.
func TestStaleWriteSameModificationTime(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(f, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}
	a := newSharedAgent(t, context.Background(), true)
	a.read(f)
	if err := os.WriteFile(f, []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if r := a.write(f, "three\n"); !isStale(r) {
		t.Fatalf("write = %v %q, want the stale refusal", r.State, respText(r))
	}
	if got := a.read(f); !strings.Contains(got, "two") {
		t.Fatalf("read served the old copy: %q", got)
	}
	if r := a.write(f, "three\n"); r.State != message.ToolResultSuccess {
		t.Fatalf("write after reading again: %s", respText(r))
	}
}

func TestOwnEditsKeepTheVersionKnown(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.go")
	if err := os.WriteFile(f, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newSharedAgent(t, context.Background(), true)
	a.read(f)
	if r := a.edit(f, "alpha", "ALPHA"); r.State != message.ToolResultSuccess {
		t.Fatalf("edit: %s", respText(r))
	}
	if r := a.run(MultiEditTool(), map[string]any{"file_path": f, "edits": []any{
		map[string]any{"old_string": "beta", "new_string": "BETA"},
	}}); r.State != message.ToolResultSuccess {
		t.Fatalf("multi edit: %s", respText(r))
	}
	patch := "--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,3 @@\n ALPHA\n BETA\n-gamma\n+GAMMA\n"
	if r := a.run(ApplyPatchTool(), map[string]any{"file_path": f, "patch": patch}); r.State != message.ToolResultSuccess {
		t.Fatalf("patch: %s", respText(r))
	}
	if r := a.write(f, "rewritten\n"); r.State != message.ToolResultSuccess {
		t.Fatalf("a write after the agent's own edits was refused: %s", respText(r))
	}
}

// TestEditOverAnotherAgentsChange: the edit itself applies to the current
// content, but the agent has not seen the other change, so a later
// whole-file write needs a read first.
func TestEditOverAnotherAgentsChange(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a.go")
	if err := os.WriteFile(f, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newSharedAgent(t, context.Background(), true)
	b := newSharedAgent(t, context.Background(), true)
	a.read(f)
	b.read(f)
	b.edit(f, "beta", "beta from b")
	if r := a.edit(f, "alpha", "alpha from a"); r.State != message.ToolResultSuccess {
		t.Fatalf("edits are not refused: %s", respText(r))
	}
	if got := mustContent(t, f); got != "alpha from a\nbeta from b\n" {
		t.Fatalf("content = %q", got)
	}
	r := a.write(f, "alpha from a\nbeta\n")
	if r.State != message.ToolResultError || !strings.Contains(respText(r), "read") {
		t.Fatalf("a write after editing an unseen version = %v %q, want a refusal", r.State, respText(r))
	}
	if got := mustContent(t, f); got != "alpha from a\nbeta from b\n" {
		t.Fatalf("the refused write changed the file: %q", got)
	}
}

func TestStaleWriteThroughABackend(t *testing.T) {
	mt := time.Now().Truncate(time.Second)
	be := &stubStatterBackend{mtime: mt, files: map[string][]byte{"notes.md": []byte("one\n")}}
	base := WithBackend(context.Background(), be)
	a := newSharedAgent(t, base, true)
	a.read("notes.md")
	be.files["notes.md"] = []byte("two\n") // same backend mtime
	if r := a.write("notes.md", "three\n"); !isStale(r) {
		t.Fatalf("backend write = %v %q, want the stale refusal", r.State, respText(r))
	}
	if got := a.read("notes.md"); !strings.Contains(got, "two") {
		t.Fatalf("backend read served the old copy: %q", got)
	}
	if r := a.edit("notes.md", "two", "TWO"); r.State != message.ToolResultSuccess {
		t.Fatalf("backend edit: %s", respText(r))
	}
	if r := a.write("notes.md", "three\n"); r.State != message.ToolResultSuccess {
		t.Fatalf("backend write after the agent's own edit: %s", respText(r))
	}

	// A backend without an mtime still records the version.
	plain := &stubStatterBackend{statErr: context.Canceled, files: map[string][]byte{"x.md": []byte("x\n")}}
	c := newSharedAgent(t, WithBackend(context.Background(), plain), true)
	c.read("x.md")
	if r := c.write("x.md", "y\n"); r.State != message.ToolResultSuccess {
		t.Fatalf("write on a backend without mtimes: %s", respText(r))
	}
}

func TestReadCacheJSONKeepsVersionsNotTheSetting(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(f, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newSharedAgent(t, context.Background(), true)
	a.read(f)
	raw, err := json.Marshal(a.rc)
	if err != nil {
		t.Fatal(err)
	}
	restored := NewReadCache(0, 0)
	restored.SetRejectStaleWrites(true)
	if err := json.Unmarshal(raw, restored); err != nil {
		t.Fatal(err)
	}
	if !restored.RejectsStaleWrites() {
		t.Fatal("restoring entries must not change the setting")
	}
	r := &sharedAgent{t: t, ctx: WithReadCache(context.Background(), restored), rc: restored}
	if got := r.write(f, "two\n"); got.State != message.ToolResultSuccess {
		t.Fatalf("a restored version must still count: %s", respText(got))
	}

	off := NewReadCache(0, 0)
	if err := json.Unmarshal(raw, off); err != nil {
		t.Fatal(err)
	}
	if off.RejectsStaleWrites() {
		t.Fatal("the setting is not saved with the entries")
	}

	// An entry saved before versions existed vouches for none.
	if err := os.WriteFile(f, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := NewReadCache(0, 0)
	old.SetRejectStaleWrites(true)
	legacy := `{"entries":[{"Lines":["one"],"UpdatedAt":"2026-01-01T00:00:00Z","Bytes":3,"FilePath":` + jsonString(f) + `}]}`
	if err := json.Unmarshal([]byte(legacy), old); err != nil {
		t.Fatal(err)
	}
	o := &sharedAgent{t: t, ctx: WithReadCache(context.Background(), old), rc: old}
	if got := o.write(f, "two\n"); !isStale(got) {
		t.Fatalf("a legacy entry = %v %q, want the stale refusal", got.State, respText(got))
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestOwnEditsThroughABackend(t *testing.T) {
	mt := time.Now().Truncate(time.Second)
	be := &stubStatterBackend{mtime: mt, files: map[string][]byte{"a.go": []byte("alpha\nbeta\n")}}
	a := newSharedAgent(t, WithBackend(context.Background(), be), true)
	a.read("a.go")
	if r := a.run(MultiEditTool(), map[string]any{"file_path": "a.go", "edits": []any{
		map[string]any{"old_string": "alpha", "new_string": "ALPHA"},
	}}); r.State != message.ToolResultSuccess {
		t.Fatalf("multi edit: %s", respText(r))
	}
	if r := a.run(ApplyPatchTool(), map[string]any{"file_path": "a.go", "patch": "@@ -1,2 +1,2 @@\n ALPHA\n-beta\n+BETA\n"}); r.State != message.ToolResultSuccess {
		t.Fatalf("patch: %s", respText(r))
	}
	if r := a.write("a.go", "rewritten\n"); r.State != message.ToolResultSuccess {
		t.Fatalf("a write after the agent's own backend edits was refused: %s", respText(r))
	}
}

func TestKnowsVersionAndHostMtime(t *testing.T) {
	rc := NewReadCache(0, 0)
	if rc.knowsVersion("missing", []byte("x")) {
		t.Fatal("an unknown path has no version")
	}
	rc.noteOwnEdit("missing", []byte("x"), []byte("y"), nil)
	if rc.Len() != 0 {
		t.Fatal("an edit of an unknown file must not create an entry")
	}
	if hostMtime(filepath.Join(t.TempDir(), "absent")) != nil {
		t.Fatal("an absent file has no mtime")
	}
}
