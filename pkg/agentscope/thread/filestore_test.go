package thread

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func openTestFileStore(t *testing.T, path string, opts ...StoreOption) *FileStore {
	t.Helper()
	s, err := OpenFileStore(path, append([]StoreOption{WithClock(fixedClock())}, opts...)...)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestFileStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "thread.jsonl")
	s := openTestFileStore(t, path) // fsync on
	ctx := context.Background()
	mustAppend(t, s, &Guard{}, ownerChange("", "edge"), replyFrom("edge", "hello"))
	mustAppend(t, s, nil, withKey(report("cloud", "keyed"), "k1"))
	// Blocks without their type tags must still come back as the same
	// block types after a reopen.
	media := Entry{Kind: KindUserInput, Content: []message.ContentBlock{
		message.TextBlock{Text: "look"},
		message.DataBlock{Source: message.Base64Source{Data: "aGk=", MediaType: "image/png"}},
		message.HintBlock{Hint: []message.ContentBlock{message.TextBlock{Text: "inner"}}},
	}}
	mustAppend(t, s, nil, media, Entry{Kind: KindSummary, Coverage: &Coverage{Through: 3}, Summary: &SummaryMeta{Target: "edge"}})
	before := mustRead(t, s, 0, Filter{}, 0)
	head := mustHead(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r := openTestFileStore(t, path)
	if h := mustHead(t, r); h != head {
		t.Fatalf("reopened head %+v, want %+v", h, head)
	}
	if h := mustHead(t, r); h != (Head{LastSeq: 5, Owner: "edge", Epoch: 1}) {
		t.Fatalf("head %+v, want edge at epoch 1 with 5 entries", h)
	}
	after := mustRead(t, r, 0, Filter{}, 0)
	if mustJSON(t, after) != mustJSON(t, before) {
		t.Fatalf("reopened entries differ:\n got %s\nwant %s", mustJSON(t, after), mustJSON(t, before))
	}
	if d, ok := after[3].Content[1].(message.DataBlock); !ok || d.GetMediaType() != "image/png" {
		t.Errorf("data block came back as %#v", after[3].Content[1])
	}
	if !reflect.DeepEqual(after[3].Content, before[3].Content) {
		t.Errorf("content after reopen %#v, want %#v", after[3].Content, before[3].Content)
	}
	if seq, err := r.Append(ctx, []Entry{withKey(report("cloud", "again"), "k1")}, nil); err != nil || seq != 3 {
		t.Errorf("retry after reopen returned %d, %v; want 3, nil", seq, err)
	}
	if _, err := r.Append(ctx, []Entry{replyFrom("cloud", "x")}, &Guard{}); !errors.Is(err, ErrNotOwner) {
		t.Errorf("stale guard after reopen: %v, want ErrNotOwner", err)
	}
	if seq := mustAppend(t, r, &Guard{Owner: "edge", Epoch: 1}, replyFrom("edge", "more")); seq != 6 {
		t.Errorf("Append after reopen returned %d, want 6", seq)
	}
}

func TestFileStoreOneLinePerBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread.jsonl")
	s := openTestFileStore(t, path, WithoutSync())
	mustAppend(t, s, nil, userInput("a"), userInput("b"), userInput("c"))
	mustAppend(t, s, nil, userInput("d"))
	lines := strings.Split(strings.TrimSuffix(string(readFile(t, path)), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want one per batch", len(lines))
	}
	for i, want := range []int{3, 1} {
		var batch []Entry
		if err := json.Unmarshal([]byte(lines[i]), &batch); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if len(batch) != want {
			t.Errorf("line %d holds %d entries, want %d", i+1, len(batch), want)
		}
	}
}

func TestFileStoreTornFinalLine(t *testing.T) {
	tails := map[string]string{
		"partial array":          `[{"seq":3,"epoch":0,"ki`,
		"garbage with newline":   "not json\n",
		"blank final line":       "\n",
		"partial with zero fill": "[{\"seq\":3\x00\x00\x00",
	}
	for name, tail := range tails {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "thread.jsonl")
			s := openTestFileStore(t, path, WithoutSync())
			mustAppend(t, s, nil, userInput("a"))
			mustAppend(t, s, nil, userInput("b"))
			_ = s.Close()
			good := readFile(t, path)
			writeFile(t, path, append(bytes.Clone(good), tail...))

			r := openTestFileStore(t, path)
			if h := mustHead(t, r); h.LastSeq != 2 {
				t.Fatalf("head %+v after a torn write, want LastSeq 2", h)
			}
			if got := readFile(t, path); !bytes.Equal(got, good) {
				t.Fatalf("torn line not truncated: file is %q", got)
			}
			if seq := mustAppend(t, r, nil, userInput("c")); seq != 3 {
				t.Fatalf("Append returned %d, want 3", seq)
			}
			_ = r.Close()
			again := openTestFileStore(t, path)
			if h := mustHead(t, again); h.LastSeq != 3 {
				t.Fatalf("head %+v after reopen, want LastSeq 3", h)
			}
		})
	}
}

func TestFileStoreFinalLineWithoutNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread.jsonl")
	s := openTestFileStore(t, path, WithoutSync())
	mustAppend(t, s, nil, userInput("a"))
	mustAppend(t, s, nil, userInput("b"))
	_ = s.Close()
	data := readFile(t, path)
	writeFile(t, path, bytes.TrimSuffix(data, []byte("\n")))

	r := openTestFileStore(t, path)
	if h := mustHead(t, r); h.LastSeq != 2 {
		t.Fatalf("head %+v, want the unterminated batch kept", h)
	}
	if got := readFile(t, path); !bytes.Equal(got, data) {
		t.Fatalf("missing newline not restored: %q", got)
	}
	mustAppend(t, r, nil, userInput("c"))
	_ = r.Close()
	if h := mustHead(t, openTestFileStore(t, path)); h.LastSeq != 3 {
		t.Fatalf("head %+v after reopen, want LastSeq 3", h)
	}
}

// rawLine encodes entries exactly as given, as one log line.
func rawLine(t *testing.T, entries ...Entry) string {
	t.Helper()
	return mustJSON(t, entries) + "\n"
}

func at(seq, epoch uint64, e Entry) Entry { //nolint:gocritic // test helper
	e.Seq, e.Epoch, e.Time = seq, epoch, testTime
	return e
}

func TestFileStoreCorrupt(t *testing.T) {
	cases := []struct {
		name string
		data func(t *testing.T) string
		want string
	}{
		{"unparseable middle line", func(t *testing.T) string {
			return rawLine(t, at(1, 0, userInput("a"))) + "garbage\n" + rawLine(t, at(2, 0, userInput("b")))
		}, "line 2"},
		{"seq gap", func(t *testing.T) string {
			return rawLine(t, at(1, 0, userInput("a"))) + rawLine(t, at(3, 0, userInput("b")))
		}, "line 2: entry has seq 3, want 2"},
		{"seq not starting at 1", func(t *testing.T) string {
			return rawLine(t, at(2, 0, userInput("a")))
		}, "line 1: entry has seq 2, want 1"},
		{"seq repeated in a batch", func(t *testing.T) string {
			return rawLine(t, at(1, 0, userInput("a")), at(1, 0, userInput("b")))
		}, "want 2"},
		{"wrong epoch", func(t *testing.T) string {
			return rawLine(t, at(1, 0, ownerChange("", "edge")))
		}, "epoch 0, want 1"},
		{"dedup key repeated across lines", func(t *testing.T) string {
			return rawLine(t, at(1, 0, withKey(userInput("a"), "k"))) + rawLine(t, at(2, 0, withKey(userInput("b"), "k")))
		}, "repeats dedup key"},
		{"dedup key repeated in a line", func(t *testing.T) string {
			return rawLine(t, at(1, 0, withKey(userInput("a"), "k")), at(2, 0, withKey(userInput("b"), "k")))
		}, "repeats dedup key"},
		{"empty batch", func(t *testing.T) string {
			return "[]\n" + rawLine(t, at(1, 0, userInput("a")))
		}, "empty batch"},
		{"null line", func(t *testing.T) string {
			return "null\n" + rawLine(t, at(1, 0, userInput("a")))
		}, "empty batch"},
		{"invalid entry", func(t *testing.T) string {
			return rawLine(t, at(1, 1, Entry{Kind: KindOwnerChange}))
		}, "owner_change needs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "thread.jsonl")
			writeFile(t, path, []byte(tc.data(t)))
			s, err := OpenFileStore(path, WithoutSync())
			if err == nil {
				_ = s.Close()
				t.Fatal("OpenFileStore succeeded on a corrupt log")
			}
			if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want ErrCorrupt containing %q", err, tc.want)
			}
		})
	}
}

func TestOpenFileStoreErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenFileStore(dir); err == nil {
		t.Error("opening a directory succeeded")
	}
	blocker := filepath.Join(dir, "file")
	writeFile(t, blocker, nil)
	if _, err := OpenFileStore(filepath.Join(blocker, "sub", "thread.jsonl")); err == nil {
		t.Error("opening under a regular file succeeded")
	}
}

func TestFileStoreSyncsNewFileDirectory(t *testing.T) {
	var synced []string
	syncLogDir = func(dir string) error { synced = append(synced, dir); return nil }
	t.Cleanup(func() { syncLogDir = syncDir })

	dir := t.TempDir()
	path := filepath.Join(dir, "thread.jsonl")
	s := openTestFileStore(t, path)
	_ = s.Close()
	if !reflect.DeepEqual(synced, []string{dir}) {
		t.Fatalf("directory syncs after creating the log = %q, want %q", synced, dir)
	}
	_ = openTestFileStore(t, path) // reopening creates nothing
	_ = openTestFileStore(t, filepath.Join(dir, "other.jsonl"), WithoutSync())
	if len(synced) != 1 {
		t.Fatalf("directory synced %d times; a reopen and WithoutSync must not sync it", len(synced))
	}

	syncLogDir = func(string) error { return errors.New("dir sync failed") }
	if _, err := OpenFileStore(filepath.Join(dir, "third.jsonl")); err == nil || !strings.Contains(err.Error(), "sync log directory") {
		t.Fatalf("OpenFileStore with a failing directory sync: %v", err)
	}
	// The real sync works or is ignored on this platform.
	if err := syncDir(dir); err != nil {
		t.Fatalf("syncDir: %v", err)
	}
	if err := syncDir(filepath.Join(dir, "missing")); err != nil && runtime.GOOS == "windows" {
		t.Fatalf("syncDir on Windows must ignore errors: %v", err)
	}
}

func TestDirSyncError(t *testing.T) {
	other := errors.New("i/o error")
	for _, tc := range []struct {
		goos string
		err  error
		want error
	}{
		{"linux", nil, nil},
		{"windows", other, nil},
		{"linux", fmt.Errorf("sync: %w", errors.ErrUnsupported), nil},
		{"linux", &os.PathError{Op: "sync", Path: "d", Err: syscall.EINVAL}, nil},
		{"linux", other, other},
	} {
		if got := dirSyncError(tc.goos, tc.err); !errors.Is(got, tc.want) || (tc.want == nil && got != nil) {
			t.Errorf("dirSyncError(%s, %v) = %v, want %v", tc.goos, tc.err, got, tc.want)
		}
	}
}

// faultyFile fails chosen operations of the file a FileStore writes.
type faultyFile struct {
	logFile
	failWrite, partialWrite, failSync, failTruncate bool
}

func (f *faultyFile) WriteAt(p []byte, off int64) (int, error) {
	if !f.failWrite {
		return f.logFile.WriteAt(p, off)
	}
	n := 0
	if f.partialWrite {
		n, _ = f.logFile.WriteAt(p[:len(p)/2], off)
	}
	return n, errors.New("disk full")
}

func (f *faultyFile) Sync() error {
	if f.failSync {
		return errors.New("sync failed")
	}
	return f.logFile.Sync()
}

func (f *faultyFile) Truncate(size int64) error {
	if f.failTruncate {
		return errors.New("truncate failed")
	}
	return f.logFile.Truncate(size)
}

func TestFileStoreWriteFailures(t *testing.T) {
	cases := []struct {
		name       string
		fault      faultyFile
		wantBroken bool
	}{
		{"write fails and is cut back", faultyFile{failWrite: true, partialWrite: true}, false},
		{"write and truncate fail", faultyFile{failWrite: true, partialWrite: true, failTruncate: true}, true},
		{"sync fails", faultyFile{failSync: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "thread.jsonl")
			s := openTestFileStore(t, path)
			mustAppend(t, s, nil, userInput("kept"))
			good := readFile(t, path)
			fault := tc.fault
			fault.logFile = s.file
			s.file = &fault

			if _, err := s.Append(context.Background(), []Entry{userInput("lost")}, nil); err == nil || !strings.Contains(err.Error(), "append to") {
				t.Fatalf("Append error %v, want a write failure", err)
			}
			if h := mustHead(t, s); h.LastSeq != 1 {
				t.Fatalf("a failed write changed the head: %+v", h)
			}
			if got := mustRead(t, s, 0, Filter{}, 0); len(got) != 1 {
				t.Fatalf("a failed write is readable: %d entries", len(got))
			}

			fault.failWrite, fault.failSync, fault.failTruncate = false, false, false
			_, err := s.Append(context.Background(), []Entry{userInput("next")}, nil)
			if tc.wantBroken {
				if err == nil || !strings.Contains(err.Error(), "unusable") {
					t.Fatalf("Append after an unrecoverable failure: %v, want the store unusable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Append after a recovered failure: %v", err)
			}
			_ = s.Close()
			got := readFile(t, path)
			if !bytes.HasPrefix(got, good) || bytes.Count(got, []byte("\n")) != 2 {
				t.Fatalf("file after recovery is %q", got)
			}
			if h := mustHead(t, openTestFileStore(t, path)); h.LastSeq != 2 {
				t.Fatalf("reopened head %+v, want LastSeq 2", h)
			}
		})
	}
}

func TestFileStoreCloseError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread.jsonl")
	s := openTestFileStore(t, path)
	f := s.file
	_ = f.Close()
	if err := s.Close(); err == nil || !strings.Contains(err.Error(), "close log") {
		t.Fatalf("Close over a closed file: %v", err)
	}
}
