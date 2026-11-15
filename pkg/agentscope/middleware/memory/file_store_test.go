package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestFileStore_AddPersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Add(ctx, "user likes Go", "u1", "agent-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(ctx, "user deploys on Fridays", "u1", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(ctx, "", "u1", ""); err != nil {
		t.Fatal(err) // empty text is a no-op
	}

	// A fresh store instance must see the persisted records.
	s2, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s2.List(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 persisted memories, got %+v", list)
	}
	if list[0].ID != "mem_1" || list[1].ID != "mem_2" {
		t.Errorf("ids must continue the mem_N sequence, got %s %s", list[0].ID, list[1].ID)
	}

	// New IDs continue past the persisted ones.
	if err := s2.Add(ctx, "third", "u1", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s2.List(ctx, "u1"); got[2].ID != "mem_3" {
		t.Errorf("next id wrong: %+v", got[2])
	}
}

func TestFileStore_SearchFilters(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir)
	ctx := context.Background()
	_ = s.Add(ctx, "the user prefers Go for services", "u1", "a")
	_ = s.Add(ctx, "the user prefers Rust for CLI tools", "u1", "b")
	_ = s.Add(ctx, "another user prefers Go", "u2", "a")

	res, err := s.Search(ctx, "go", "u1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !strings.Contains(res[0].Text, "Go for services") {
		t.Fatalf("search must match user + keyword, got %+v", res)
	}

	// Agent scoping.
	res, err = s.Search(ctx, "prefers", "u1", &SearchOptions{AgentID: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !strings.Contains(res[0].Text, "Rust") {
		t.Fatalf("agent-scoped search wrong: %+v", res)
	}

	// TopK honored.
	res, _ = s.Search(ctx, "prefers", "u1", &SearchOptions{TopK: 1})
	if len(res) != 1 {
		t.Fatalf("topK ignored: %+v", res)
	}
}

func TestFileStore_DeletePersists(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir)
	ctx := context.Background()
	_ = s.Add(ctx, "keep me", "u1", "")
	_ = s.Add(ctx, "drop me", "u1", "")

	if err := s.Delete(ctx, "mem_2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "ghost"); err == nil {
		t.Error("deleting a missing id must error")
	}

	s2, _ := NewFileStore(dir)
	list, err := s2.List(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Text != "keep me" {
		t.Fatalf("delete must persist, got %+v", list)
	}
}

func TestFileStore_DeleteFailurePreservesSnapshot(t *testing.T) {
	for _, tc := range []struct{ count, target int }{{3, 1}, {3, 2}, {3, 3}, {1, 1}} {
		t.Run(fmt.Sprintf("%d_of_%d", tc.target, tc.count), func(t *testing.T) {
			dir := t.TempDir()
			s, err := NewFileStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			for i := 1; i <= tc.count; i++ {
				if err := s.Add(ctx, fmt.Sprintf("memory %d", i), "u1", "a"); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.List(ctx, "u1")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, FileStoreFilename)
			backup := path + ".saved"
			if err := os.Rename(path, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			id := fmt.Sprintf("mem_%d", tc.target)
			if err := s.Delete(ctx, id); err == nil {
				t.Fatal("replacing a directory must fail")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(backup, path); err != nil {
				t.Fatal(err)
			}
			after, err := s.List(ctx, "u1")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("failed delete changed the snapshot: before=%+v after=%+v", before, after)
			}
			if err := s.Delete(ctx, id); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if err := s.Add(ctx, "after retry", "u1", "a"); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewFileStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := reopened.List(ctx, "u1")
			if err != nil {
				t.Fatal(err)
			}
			if len(persisted) != tc.count || persisted[len(persisted)-1].ID != fmt.Sprintf("mem_%d", tc.count+1) {
				t.Fatalf("retry/add did not persist or continue IDs: %+v", persisted)
			}
			for _, m := range persisted {
				if m.ID == id {
					t.Fatalf("deleted memory survived retry: %+v", m)
				}
			}
		})
	}
}

func TestFileStore_SkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	raw := `{"id":"mem_1","text":"good","user_id":"u1"}
{torn json from a crash
{"id":"mem_2","text":"also good","user_id":"u1"}
`
	if err := os.WriteFile(filepath.Join(dir, FileStoreFilename), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.List(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("corrupt line must be skipped, got %+v", list)
	}
	// IDs continue past the surviving records.
	if err := s.Add(context.Background(), "new", "u1", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.List(context.Background(), "u1"); got[2].ID != "mem_3" {
		t.Errorf("id continuation wrong: %+v", got[2])
	}
}

func TestFileStore_ConcurrentAdds(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = s.Add(ctx, "memory "+strings.Repeat("x", n%5), "u1", "")
		}(i)
	}
	wg.Wait()

	list, err := s.List(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 20 {
		t.Fatalf("concurrent adds lost records: got %d", len(list))
	}
	// Every record persisted exactly once with a unique ID.
	ids := map[string]bool{}
	for _, m := range list {
		if ids[m.ID] {
			t.Fatalf("duplicate id %s", m.ID)
		}
		ids[m.ID] = true
	}
}

func TestNewFileStore_RequiresDir(t *testing.T) {
	if _, err := NewFileStore(""); err == nil {
		t.Error("empty dir must error")
	}
}

func TestFileStore_OverlongLineFailsWholeLoad(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString(`{"id":"mem_1","text":"good","user_id":"u1"}` + "\n")
	// One line over the 4 MiB scanner cap.
	b.WriteString(`{"id":"mem_2","text":"` + strings.Repeat("x", 5<<20) + `"}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, FileStoreFilename), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.List(ctx, "u1"); err == nil {
		t.Fatal("over-long line must fail the load, not silently drop it")
	}
	// Nothing partial may be observable afterwards.
	if list, err := s.List(ctx, "u1"); err == nil && len(list) != 0 {
		t.Fatalf("failed load must commit nothing, got %+v", list)
	}
}
