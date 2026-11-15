package embedding

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
)

func TestCacheOversizePreservesExistingEntries(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "replace"}[sameKey], func(t *testing.T) {
			dir := t.TempDir()
			c, err := NewFileEmbeddingCache(dir, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			small := [][]float32{{1, 2}}
			if err := c.Store("small", small); err != nil {
				t.Fatal(err)
			}
			large := [][]float32{make([]float32, 600000)}
			key := "large"
			if sameKey {
				key = "small"
			}
			if err := c.Store(key, large); err != nil {
				t.Fatal(err)
			}
			if got, ok := c.Retrieve("small"); !ok || !reflect.DeepEqual(got, small) {
				t.Fatalf("old value lost: %v %t", got, ok)
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 {
				t.Fatalf("files=%v err=%v", files, err)
			}
		})
	}
}
func TestCacheSizeBoundaryAndOverflow(t *testing.T) {
	// [[0,...,0,10]] encodes to exactly 1 MiB.
	exact := [][]float32{make([]float32, (1024*1024-4)/2)}
	exact[0][0] = 10
	data, err := json.Marshal(exact)
	if err != nil || len(data) != 1024*1024 {
		t.Fatalf("fixture bytes=%d err=%v", len(data), err)
	}
	for _, limit := range []int{1, 0, -1, int(^uint(0) >> 1)} {
		c, err := NewFileEmbeddingCache(t.TempDir(), 0, limit)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Store("exact", exact); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.Retrieve("exact"); !ok {
			t.Fatalf("limit %d discarded fitting entry", limit)
		}
	}
}
func TestCacheAtomicReplacement(t *testing.T) {
	dir := t.TempDir()
	c, err := NewFileEmbeddingCache(dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Store("entry", [][]float32{{1}}); err != nil {
		t.Fatal(err)
	}
	// An open descriptor must keep reading the complete old inode after replacement.
	old, err := os.Open(filepath.Join(dir, "entry.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if runtime.GOOS == "windows" {
		_ = old.Close()
	}
	if err := c.Store("entry", [][]float32{{2, 3}}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		var previous [][]float32
		if err := json.NewDecoder(old).Decode(&previous); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(previous, [][]float32{{1}}) {
			t.Fatalf("old file was overwritten: %v", previous)
		}
	}
	if err := c.Store("entry", [][]float32{{float32(math.NaN())}}); err == nil {
		t.Fatal("expected marshal error")
	}
	if got, ok := c.Retrieve("entry"); !ok || !reflect.DeepEqual(got, [][]float32{{2, 3}}) {
		t.Fatal("marshal failure lost value")
	}
	if err := os.Mkdir(filepath.Join(dir, "blocked.json"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := c.Store("blocked", [][]float32{{1}}); err == nil {
		t.Fatal("expected replacement failure")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("temporary files remain: %v", entries)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if err := c.Store("entry", [][]float32{{4}}); err != nil {
					t.Error(err)
				}
				if _, ok := c.Retrieve("entry"); !ok {
					t.Error("missing concurrent entry")
				}
			}
		}()
	}
	wg.Wait()
}
