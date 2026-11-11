package thread

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
)

// ErrCorrupt is returned when a log file cannot be replayed: a line before
// the last does not parse, or the entries break the seq, epoch or dedup
// rules the store enforces.
var ErrCorrupt = errors.New("thread: log file is corrupt")

// logFile is the part of *os.File a FileStore writes through.
type logFile interface {
	io.WriterAt
	Truncate(size int64) error
	Sync() error
	Close() error
}

// FileStore is a Store kept in a JSON Lines file. Each line is the JSON
// array of one Append batch, so a torn write loses a whole batch, never part
// of one. The whole log is also held in memory for reads.
type FileStore struct {
	mu     sync.RWMutex
	path   string
	file   logFile
	size   int64 // bytes of the file that hold accepted batches
	noSync bool
	state  *logState
	closed bool
	broken error // set when a failed write may have left the file inconsistent
}

var _ Store = (*FileStore)(nil)

// OpenFileStore opens or creates the log at path and replays it. A final
// line that does not parse is a torn write and is truncated away; any other
// unreadable line, or entries whose seqs are not contiguous from 1, fail with
// ErrCorrupt. The parent directory is created when missing.
//
// When it creates the file, and WithoutSync was not given, it also syncs the
// parent directory so the new file's directory entry survives a crash. That
// step is best effort: Windows cannot sync a directory handle, and some file
// systems reject a directory sync as unsupported or invalid; those errors are
// ignored. Any other directory sync error fails the open.
func OpenFileStore(path string, opts ...StoreOption) (*FileStore, error) {
	cfg := newStoreConfig(opts)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("thread: create log directory: %w", err)
	}
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, fs.ErrNotExist)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("thread: open log: %w", err)
	}
	if created && !cfg.noSync {
		if err := syncLogDir(dir); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("thread: sync log directory: %w", err)
		}
	}
	s, err := openLog(path, f, cfg)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return s, nil
}

// syncLogDir is the directory sync OpenFileStore uses; tests replace it.
var syncLogDir = syncDir

// syncDir syncs a directory, ignoring the errors of platforms and file
// systems that cannot sync one (see dirSyncError).
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err == nil {
		err = d.Sync()
		_ = d.Close()
	}
	return dirSyncError(runtime.GOOS, err)
}

// dirSyncError drops the errors a directory sync returns where it is not
// possible: every error on Windows (opening or flushing a directory handle
// fails there with an access or invalid-handle error), and an unsupported or
// EINVAL error elsewhere.
func dirSyncError(goos string, err error) error {
	if err == nil || goos == "windows" || errors.Is(err, errors.ErrUnsupported) || errors.Is(err, syscall.EINVAL) {
		return nil
	}
	return err
}

func openLog(path string, f *os.File, cfg storeConfig) (*FileStore, error) {
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("thread: read log: %w", err)
	}
	state := newLogState(cfg.clock)
	size, err := replayLog(state, data)
	if err != nil {
		return nil, err
	}
	changed := false
	if size < int64(len(data)) {
		if err := f.Truncate(size); err != nil {
			return nil, fmt.Errorf("thread: truncate torn log line: %w", err)
		}
		changed = true
	}
	if size > 0 && data[size-1] != '\n' {
		// The last batch was written whole but its newline was not.
		if _, err := f.WriteAt([]byte{'\n'}, size); err != nil {
			return nil, fmt.Errorf("thread: end log line: %w", err)
		}
		size++
		changed = true
	}
	if changed && !cfg.noSync {
		if err := f.Sync(); err != nil {
			return nil, fmt.Errorf("thread: sync log: %w", err)
		}
	}
	return &FileStore{path: path, file: f, size: size, noSync: cfg.noSync, state: state}, nil
}

// replayLog applies every batch in data to state and returns the length of
// the prefix that holds them. A final line that does not parse is left out.
func replayLog(state *logState, data []byte) (int64, error) {
	pos, lineNo := 0, 0
	for pos < len(data) {
		lineNo++
		end, next := len(data), len(data)
		if i := bytes.IndexByte(data[pos:], '\n'); i >= 0 {
			end, next = pos+i, pos+i+1
		}
		var batch []Entry
		if err := json.Unmarshal(data[pos:end], &batch); err != nil {
			if next >= len(data) {
				return int64(pos), nil
			}
			return 0, fmt.Errorf("%w: line %d: %w", ErrCorrupt, lineNo, err)
		}
		if err := state.replay(batch); err != nil {
			return 0, fmt.Errorf("%w: line %d: %w", ErrCorrupt, lineNo, err)
		}
		pos = next
	}
	return int64(pos), nil
}

// Append implements Store. The batch is written as one line and, unless
// WithoutSync was given, synced before Append returns.
//
// When the sync fails, Append returns an error and the store refuses every
// later write, but the batch may still be durable: the file is cut back to
// its previous length without a sync, so after a crash the batch can be
// there although the caller was told it failed. Callers that retry after a
// restart must use dedup keys, which make such a retry write nothing.
func (s *FileStore) Append(ctx context.Context, entries []Entry, g *Guard) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	if s.broken != nil {
		return 0, fmt.Errorf("thread: log %s is unusable after a failed write: %w", s.path, s.broken)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	planned, retry, err := s.state.plan(entries, g)
	if err != nil {
		return 0, err
	}
	if planned == nil {
		return retry, nil
	}
	line, err := json.Marshal(planned)
	if err != nil {
		return 0, fmt.Errorf("thread: encode batch: %w", err)
	}
	line = append(line, '\n')
	if err := s.write(line); err != nil {
		return 0, err
	}
	s.state.apply(planned)
	return planned[len(planned)-1].Seq, nil
}

// write appends one line at the end of the accepted data. On failure it
// cuts the file back; when it cannot, or when a sync failed and the file's
// durable contents are unknown (the line may be durable, and the cut is not
// synced), the store refuses further writes.
func (s *FileStore) write(line []byte) error {
	_, err := s.file.WriteAt(line, s.size)
	syncFailed := false
	if err == nil && !s.noSync {
		if err = s.file.Sync(); err != nil {
			syncFailed = true
		}
	}
	if err == nil {
		s.size += int64(len(line))
		return nil
	}
	if terr := s.file.Truncate(s.size); terr != nil {
		s.broken = errors.Join(err, terr)
	} else if syncFailed {
		s.broken = err
	}
	return fmt.Errorf("thread: append to %s: %w", s.path, err)
}

// Read implements Store.
func (s *FileStore) Read(ctx context.Context, after uint64, f Filter, limit int) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.state.read(after, &f, limit), nil
}

// Get implements Store.
func (s *FileStore) Get(ctx context.Context, seq uint64) (Entry, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return Entry{}, false, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	e, ok := s.state.get(seq)
	return e, ok, nil
}

// Head implements Store.
func (s *FileStore) Head(ctx context.Context) (Head, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return Head{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Head{}, err
	}
	return s.state.head, nil
}

// Close implements Store. It closes the file; a second Close returns
// ErrClosed.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.closed = true
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("thread: close log: %w", err)
	}
	return nil
}
