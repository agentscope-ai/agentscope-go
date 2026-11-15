package embedding

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/fsutil"
)

// FileEmbeddingCache stores embedding vectors as JSON files in a directory,
// using SHA-256 hashes as filenames. Supports eviction by file count and
// total size.
type FileEmbeddingCache struct {
	dir       string
	maxFiles  int // <=0 means unlimited
	maxSizeMB int // <=0 means unlimited
	mu        sync.Mutex
}

// NewFileEmbeddingCache creates a cache backed by the given directory.
// maxFiles and maxSizeMB control eviction; 0 disables each limit.
func NewFileEmbeddingCache(dir string, maxFiles, maxSizeMB int) (*FileEmbeddingCache, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("embedding cache: create dir: %w", err)
	}
	return &FileEmbeddingCache{
		dir:       dir,
		maxFiles:  maxFiles,
		maxSizeMB: maxSizeMB,
	}, nil
}

// Store makes a best-effort cache write. An encoded entry larger than the
// size limit is skipped, returning nil and preserving any previous value.
// Use content-addressed keys; a skipped write does not invalidate an old key.
func (c *FileEmbeddingCache) Store(key string, embeddings [][]float32) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := json.Marshal(embeddings)
	if err != nil {
		return fmt.Errorf("embedding cache: marshal: %w", err)
	}

	if c.maxSizeMB > 0 && int64(len(data)) > c.maxBytes() {
		return nil
	}

	path := filepath.Join(c.dir, key+".json")
	if err := fsutil.WriteFileAtomic(path, data, 0644); err != nil {
		return fmt.Errorf("embedding cache: write: %w", err)
	}

	c.maintain()
	return nil
}

func (c *FileEmbeddingCache) Retrieve(key string) ([][]float32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	path := filepath.Join(c.dir, key+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}

	var embeddings [][]float32
	if err := json.Unmarshal(data, &embeddings); err != nil {
		return nil, false
	}
	return embeddings, true
}

func (c *FileEmbeddingCache) Remove(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return os.Remove(filepath.Join(c.dir, key+".json"))
}

func (c *FileEmbeddingCache) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return fmt.Errorf("embedding cache: read dir: %w", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			_ = os.Remove(filepath.Join(c.dir, e.Name()))
		}
	}
	return nil
}

// maintain evicts oldest files when limits are exceeded. Must be called with mu held.
func (c *FileEmbeddingCache) maintain() {
	if c.maxFiles <= 0 && c.maxSizeMB <= 0 {
		return
	}

	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}

	type fileEntry struct {
		name    string
		size    int64
		modTime int64
	}

	var files []fileEntry
	var totalSize int64
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileEntry{
			name:    e.Name(),
			size:    info.Size(),
			modTime: info.ModTime().UnixNano(),
		})
		totalSize += info.Size()
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime < files[j].modTime
	})

	maxBytes := c.maxBytes()

	for len(files) > 0 {
		overFileLimit := c.maxFiles > 0 && len(files) > c.maxFiles
		overSizeLimit := c.maxSizeMB > 0 && totalSize > maxBytes
		if !overFileLimit && !overSizeLimit {
			break
		}
		oldest := files[0]
		_ = os.Remove(filepath.Join(c.dir, oldest.name))
		totalSize -= oldest.size
		files = files[1:]
	}
}

// maxBytes saturates before multiplying so large positive limits stay unbounded
// by representable file sizes, including on 32-bit platforms.
func (c *FileEmbeddingCache) maxBytes() int64 {
	if c.maxSizeMB <= 0 || int64(c.maxSizeMB) > math.MaxInt64/(1024*1024) {
		return math.MaxInt64
	}
	return int64(c.maxSizeMB) * 1024 * 1024
}
