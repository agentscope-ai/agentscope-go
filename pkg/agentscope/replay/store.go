package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Store persists and loads tapes.
type Store interface {
	Save(ctx context.Context, name string, tape *Tape) error
	Load(ctx context.Context, name string) (*Tape, error)
	List(ctx context.Context) ([]string, error)
}

// FileStore saves tapes as JSON files in a directory.
type FileStore struct {
	dir string
}

// NewFileStore creates a new FileStore that persists tapes in the given directory.
// The directory is created if it does not exist.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("replay: create store directory: %w", err)
	}
	return &FileStore{dir: dir}, nil
}

// Save writes a tape to a JSON file named <name>.json in the store directory.
func (s *FileStore) Save(_ context.Context, name string, tape *Tape) error {
	data, err := json.MarshalIndent(tape, "", "  ")
	if err != nil {
		return fmt.Errorf("replay: marshal tape: %w", err)
	}
	path := filepath.Join(s.dir, name+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("replay: write tape file: %w", err)
	}
	return nil
}

// Load reads a tape from a JSON file named <name>.json in the store directory.
func (s *FileStore) Load(_ context.Context, name string) (*Tape, error) {
	path := filepath.Join(s.dir, name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("replay: read tape file: %w", err)
	}
	var tape Tape
	if err := json.Unmarshal(data, &tape); err != nil {
		return nil, fmt.Errorf("replay: unmarshal tape: %w", err)
	}
	return &tape, nil
}

// List returns all tape names (without .json extension) in the store directory.
func (s *FileStore) List(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("replay: list tapes: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	return names, nil
}
