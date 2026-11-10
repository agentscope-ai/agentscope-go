package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"time"
)

const (
	DefaultMaxCacheFiles = 100
	DefaultMaxCacheBytes = 25000 * 1024 // 25 MB
)

// ReadCacheEntry holds the cached content and metadata for a single file.
type ReadCacheEntry struct {
	Lines     []string
	UpdatedAt time.Time // file mtime when cached
	Bytes     int       // total byte size of cached lines
	FilePath  string
	// Hash (EXPERIMENTAL) is the SHA-256 (hex) of the content the entry was
	// read from, or of the agent's own edit of it (SetRejectStaleWrites).
	// Empty for entries cached through CacheFile and for entries saved by
	// older versions.
	Hash string `json:"Hash,omitempty"`
}

// ReadCache is a FIFO-evicting cache for file contents.
// It limits both the number of entries and total byte size.
type ReadCache struct {
	entries       []ReadCacheEntry
	maxCacheFiles int
	maxCacheBytes int
	rejectStale   bool
	mu            sync.Mutex
}

// staleWriteMessage is the refusal of a write over a version the agent has
// not seen.
const staleWriteMessage = "the file has changed since you last read it (another agent or process may have written it); " +
	"read it again, then write"

// SetRejectStaleWrites (EXPERIMENTAL) makes the Write tool refuse to replace
// a file whose current content is not the version this cache recorded for
// it: the version the agent's last Read returned, or the result of its own
// last Edit, MultiEdit or ApplyPatch of it. Use it when agents with separate
// caches share a workspace, as a cloud and an edge agent working on one task
// can: a whole-file write from an old copy would delete what the other agent
// wrote since. The agent then reads the file again and retries. Creating a
// new file is allowed, and so is a write that leaves the content unchanged
// (the existing guard still asks for a read of a file never read).
//
// While it is on, Read always reads the file and records the version it
// returned instead of serving a cached copy, and the edit tools record their
// own result when they edited the version the cache knew (otherwise they
// drop the entry, so the agent reads before its next Edit, MultiEdit or
// Write). Edits apply to the current content and are not refused. The check
// runs just before the write and is not a lock: a write that lands in
// between is not detected. Files written by other tools, such as a shell
// command, are seen only through their content. The setting is configuration
// and is not saved with the cache's JSON.
func (rc *ReadCache) SetRejectStaleWrites(on bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.rejectStale = on
}

// RejectsStaleWrites (EXPERIMENTAL) reports whether SetRejectStaleWrites is
// on.
func (rc *ReadCache) RejectsStaleWrites() bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.rejectStale
}

// contentHash is the version key of a file's content.
func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// knowsVersion reports whether data is the version of filePath this cache
// recorded. An entry without a hash vouches for no version.
func (rc *ReadCache) knowsVersion(filePath string, data []byte) bool {
	h := contentHash(data)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, e := range rc.entries {
		if e.FilePath == filePath {
			return e.Hash != "" && e.Hash == h
		}
	}
	return false
}

// noteOwnEdit records an edit tool's result. When before is the version the
// cache knew, after becomes it; otherwise the file changed under the agent
// and its entry is dropped. mtime is the file's time after the write; nil
// stores a zero time, so the next freshness check misses and Read reads the
// file again.
func (rc *ReadCache) noteOwnEdit(filePath string, before, after []byte, mtime *time.Time) {
	knew := rc.knowsVersion(filePath, before)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for i, e := range rc.entries {
		if e.FilePath == filePath {
			rc.removeAt(i)
			break
		}
	}
	if !knew {
		return
	}
	var updatedAt time.Time
	if mtime != nil {
		updatedAt = *mtime
	}
	rc.insertLocked(filePath, splitReadLines(after), updatedAt, contentHash(after))
}

// hostMtime is the modification time of a host file, nil when it cannot be
// read.
func hostMtime(path string) *time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	t := info.ModTime()
	return &t
}

// NewReadCache creates a ReadCache with the given limits.
// Zero values for limits use defaults (100 files, 25MB).
func NewReadCache(maxFiles, maxBytes int) *ReadCache {
	if maxFiles <= 0 {
		maxFiles = DefaultMaxCacheFiles
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxCacheBytes
	}
	return &ReadCache{
		maxCacheFiles: maxFiles,
		maxCacheBytes: maxBytes,
	}
}

// The returned *ReadCacheEntry is always a copy: callers may hold it after
// the lock is released without racing eviction or Remove. Its Lines slice is
// shared but never mutated once cached.

// GetCache returns the cached entry for the given file path.
// It validates freshness by checking the file's mtime on the HOST
// filesystem. If the file has been modified since caching, the stale entry
// is removed and nil is returned.
func (rc *ReadCache) GetCache(filePath string) *ReadCacheEntry {
	return rc.GetCacheWithMtime(filePath, nil)
}

// GetCacheWithMtime is GetCache with an explicit freshness mtime obtained
// from the same filesystem that served the read — e.g. a workspace backend
// (upstream #2092). A nil mtime falls back to a host os.Stat.
//
// The freshness probe runs OUTSIDE the lock (a backend stat can be a
// network round-trip; serializing all cache reads behind it was already
// undesirable for host stats), then the entry is re-located and validated
// under the lock.
func (rc *ReadCache) GetCacheWithMtime(filePath string, mtime *time.Time) *ReadCacheEntry {
	rc.mu.Lock()
	found := false
	for _, e := range rc.entries {
		if e.FilePath == filePath {
			found = true
			break
		}
	}
	rc.mu.Unlock()
	if !found {
		return nil
	}

	var actual time.Time
	if mtime != nil {
		actual = *mtime
	} else {
		info, err := os.Stat(filePath)
		if err != nil {
			rc.Remove(filePath)
			return nil
		}
		actual = info.ModTime()
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()
	for i, e := range rc.entries {
		if e.FilePath != filePath {
			continue
		}
		if !actual.Equal(e.UpdatedAt) {
			rc.removeAt(i)
			return nil
		}
		// Upstream #1811: a hit refreshes recency. Without this a
		// repeatedly-read hot file ages out first under FIFO eviction.
		entry := rc.entries[i]
		rc.removeAt(i)
		rc.entries = append(rc.entries, entry)
		// Return a COPY. removeAt shifts elements within the shared backing
		// array, so a pointer into rc.entries would be read by the caller
		// while a concurrent Remove (from Write/Edit, upstream #2092) moves
		// that memory. Concurrent tool batches make Read+Write on one file a
		// realistic pairing.
		out := rc.entries[len(rc.entries)-1]
		return &out
	}
	return nil // evicted between the probe and the re-lock
}

// Remove drops the cached entry for filePath, if any. Write/Edit call it
// after modifying a file through a backend whose filesystem the cache
// cannot stat from the host (upstream #2092).
func (rc *ReadCache) Remove(filePath string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for i, e := range rc.entries {
		if e.FilePath == filePath {
			rc.removeAt(i)
			return
		}
	}
}

// HasBeenRead returns true if the file has an entry in the cache (even if stale).
// Used by Write/Edit as a "read before write" guard without full freshness check.
func (rc *ReadCache) HasBeenRead(filePath string) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	for _, e := range rc.entries {
		if e.FilePath == filePath {
			return true
		}
	}
	return false
}

// CacheFile stores file content in the cache, evicting oldest entries if
// needed. Freshness metadata comes from a host os.Stat.
func (rc *ReadCache) CacheFile(filePath string, lines []string) {
	rc.CacheFileWithMtime(filePath, lines, nil)
}

// CacheFileWithMtime is CacheFile with an explicit mtime obtained from the
// same filesystem that served the read (upstream #2092). A nil mtime falls
// back to a host os.Stat, and a failed stat skips caching entirely.
func (rc *ReadCache) CacheFileWithMtime(filePath string, lines []string, mtime *time.Time) {
	rc.cacheFile(filePath, lines, mtime, "")
}

// cacheFile is CacheFileWithMtime that also records the content's hash.
func (rc *ReadCache) cacheFile(filePath string, lines []string, mtime *time.Time, hash string) {
	var updatedAt time.Time
	if mtime != nil {
		updatedAt = *mtime
	} else {
		info, err := os.Stat(filePath)
		if err != nil {
			return
		}
		updatedAt = info.ModTime()
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()

	for i, e := range rc.entries {
		if e.FilePath == filePath {
			rc.removeAt(i)
			break
		}
	}
	rc.insertLocked(filePath, lines, updatedAt, hash)
}

// insertLocked appends an entry for a path that has none, evicting the
// oldest entries over the limits. Caller holds rc.mu.
func (rc *ReadCache) insertLocked(filePath string, lines []string, updatedAt time.Time, hash string) {
	byteSize := 0
	for _, line := range lines {
		byteSize += len(line)
	}

	for len(rc.entries) >= rc.maxCacheFiles {
		rc.removeAt(0)
	}

	totalBytes := rc.totalBytes()
	for len(rc.entries) > 0 && totalBytes+byteSize > rc.maxCacheBytes {
		totalBytes -= rc.entries[0].Bytes
		rc.removeAt(0)
	}

	rc.entries = append(rc.entries, ReadCacheEntry{
		Lines:     lines,
		UpdatedAt: updatedAt,
		Bytes:     byteSize,
		FilePath:  filePath,
		Hash:      hash,
	})
}

// CleanFileCache removes all entries except those whose FilePath is in the given set.
func (rc *ReadCache) CleanFileCache(keepPaths map[string]bool) {
	if keepPaths == nil {
		rc.mu.Lock()
		rc.entries = nil
		rc.mu.Unlock()
		return
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()

	var kept []ReadCacheEntry
	for _, e := range rc.entries {
		if keepPaths[e.FilePath] {
			kept = append(kept, e)
		}
	}
	rc.entries = kept
}

// Len returns the number of cached entries.
func (rc *ReadCache) Len() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.entries)
}

func (rc *ReadCache) removeAt(i int) {
	rc.entries = append(rc.entries[:i], rc.entries[i+1:]...)
}

func (rc *ReadCache) totalBytes() int {
	total := 0
	for _, e := range rc.entries {
		total += e.Bytes
	}
	return total
}

// readCacheJSON is the JSON-friendly representation of ReadCache.
type readCacheJSON struct {
	Entries       []ReadCacheEntry `json:"entries"`
	MaxCacheFiles int              `json:"max_cache_files"`
	MaxCacheBytes int              `json:"max_cache_bytes"`
}

// MarshalJSON serializes ReadCache to JSON.
func (rc *ReadCache) MarshalJSON() ([]byte, error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return json.Marshal(readCacheJSON{
		Entries:       rc.entries,
		MaxCacheFiles: rc.maxCacheFiles,
		MaxCacheBytes: rc.maxCacheBytes,
	})
}

// UnmarshalJSON deserializes ReadCache from JSON.
func (rc *ReadCache) UnmarshalJSON(data []byte) error {
	var raw readCacheJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.entries = raw.Entries
	rc.maxCacheFiles = raw.MaxCacheFiles
	if rc.maxCacheFiles <= 0 {
		rc.maxCacheFiles = DefaultMaxCacheFiles
	}
	rc.maxCacheBytes = raw.MaxCacheBytes
	if rc.maxCacheBytes <= 0 {
		rc.maxCacheBytes = DefaultMaxCacheBytes
	}
	return nil
}

type readCacheKey struct{}

// WithReadCache attaches a ReadCache to a Go context.
func WithReadCache(ctx context.Context, rc *ReadCache) context.Context {
	return context.WithValue(ctx, readCacheKey{}, rc)
}

// GetReadCache retrieves the ReadCache from a Go context, or nil if none.
func GetReadCache(ctx context.Context) *ReadCache {
	v, _ := ctx.Value(readCacheKey{}).(*ReadCache)
	return v
}
