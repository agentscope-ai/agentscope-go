package runtime

import (
	"os"
	"sort"
	"testing"
	"time"
)

func TestInMemorySessionStoreSaveLoad(t *testing.T) {
	s := NewInMemorySessionStore()
	state := &SessionState{
		ID:        "sess-1",
		Metadata:  map[string]any{"key": "value"},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := s.Save("sess-1", state); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.Load("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != "sess-1" {
		t.Errorf("ID = %q, want %q", loaded.ID, "sess-1")
	}
	if loaded.Metadata["key"] != "value" {
		t.Errorf("Metadata[key] = %v, want %q", loaded.Metadata["key"], "value")
	}
}

func TestInMemorySessionStoreNotFound(t *testing.T) {
	s := NewInMemorySessionStore()
	_, err := s.Load("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestInMemorySessionStoreDelete(t *testing.T) {
	s := NewInMemorySessionStore()
	s.Save("sess-1", &SessionState{ID: "sess-1"})
	s.Delete("sess-1")

	_, err := s.Load("sess-1")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestInMemorySessionStoreList(t *testing.T) {
	s := NewInMemorySessionStore()
	s.Save("a", &SessionState{ID: "a"})
	s.Save("b", &SessionState{ID: "b"})

	ids, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Errorf("List = %v, want [a, b]", ids)
	}
}

func TestFileSessionStoreSaveLoad(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSessionStore(dir)

	state := &SessionState{
		ID:        "sess-file-1",
		Metadata:  map[string]any{"env": "test"},
		CreatedAt: time.Now().Truncate(time.Second),
		UpdatedAt: time.Now().Truncate(time.Second),
	}

	if err := s.Save("sess-file-1", state); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.Load("sess-file-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != "sess-file-1" {
		t.Errorf("ID = %q, want %q", loaded.ID, "sess-file-1")
	}
}

func TestFileSessionStoreNotFound(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSessionStore(dir)

	_, err := s.Load("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestFileSessionStoreDelete(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSessionStore(dir)

	s.Save("del", &SessionState{ID: "del"})
	if err := s.Delete("del"); err != nil {
		t.Fatal(err)
	}

	_, err := s.Load("del")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestFileSessionStoreList(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSessionStore(dir)

	s.Save("x", &SessionState{ID: "x"})
	s.Save("y", &SessionState{ID: "y"})

	ids, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "x" || ids[1] != "y" {
		t.Errorf("List = %v, want [x, y]", ids)
	}
}

func TestFileSessionStoreListEmptyDir(t *testing.T) {
	s := NewFileSessionStore("/nonexistent/path/" + t.Name())
	ids, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("List = %v, want empty", ids)
	}
}

func TestFileSessionStoreWriteCreatesDir(t *testing.T) {
	dir := t.TempDir()
	nested := dir + "/sub/sessions"
	s := NewFileSessionStore(nested)

	if err := s.Save("test", &SessionState{ID: "test"}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(nested + "/test/session.json"); err != nil {
		t.Errorf("session.json not created: %v", err)
	}
}
