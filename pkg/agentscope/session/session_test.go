package session_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/session"
)

func openJSONSession(t *testing.T, path string) *session.JSONSession {
	t.Helper()
	s, err := session.NewJSONSession(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func assertEntry(t *testing.T, s *session.JSONSession, key string, value any, present bool) {
	t.Helper()
	got, err := s.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := s.List(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if got != value || slices.Contains(keys, key) != present {
		t.Errorf("entry %q = (%v, present=%t), want (%v, present=%t)", key, got, slices.Contains(keys, key), value, present)
	}
}

func TestJSONSessionFailedSetPreservesEntry(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   any
		present bool
	}{
		{name: "absent"},
		{name: "existing", value: "saved", present: true},
		{name: "existing nil", present: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "session.json")
			s := openJSONSession(t, path)
			if test.present {
				if err := s.Set(ctx, "entry", test.value); err != nil {
					t.Fatal(err)
				}
			}
			var unsupported *json.UnsupportedTypeError
			if err := s.Set(ctx, "entry", make(chan int)); !errors.As(err, &unsupported) {
				t.Fatalf("Set returned %v, want JSON encoding error", err)
			}
			assertEntry(t, s, "entry", test.value, test.present)
			assertEntry(t, openJSONSession(t, path), "entry", test.value, test.present)
			if err := s.Set(ctx, "later", "valid"); err != nil {
				t.Fatalf("Set after failed write: %v", err)
			}
			reopened := openJSONSession(t, path)
			assertEntry(t, reopened, "entry", test.value, test.present)
			assertEntry(t, reopened, "later", "valid", true)
		})
	}
}

func TestJSONSessionFailedDeletePreservesEntry(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "session.json")
	s := openJSONSession(t, path)
	if err := s.Set(ctx, "entry", "saved"); err != nil {
		t.Fatal(err)
	}
	backup := path + ".kept"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	var linkErr *os.LinkError
	if err := s.Delete(ctx, "entry"); !errors.As(err, &linkErr) || linkErr.Op != "rename" || linkErr.New != path {
		t.Fatalf("Delete returned %v, want rename failure for %s", err, path)
	}
	assertEntry(t, s, "entry", "saved", true)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "later", "valid"); err != nil {
		t.Fatal(err)
	}
	reopened := openJSONSession(t, path)
	assertEntry(t, reopened, "entry", "saved", true)
	assertEntry(t, reopened, "later", "valid", true)
	if err := reopened.Delete(ctx, "entry"); err != nil {
		t.Fatal(err)
	}
	assertEntry(t, openJSONSession(t, path), "entry", nil, false)
}

func TestJSONSessionWithoutPath(t *testing.T) {
	s := openJSONSession(t, "")
	ch := make(chan int)
	if err := s.Set(t.Context(), "entry", ch); err != nil {
		t.Fatal(err)
	}
	assertEntry(t, s, "entry", ch, true)
	if err := s.Delete(t.Context(), "entry"); err != nil {
		t.Fatal(err)
	}
	assertEntry(t, s, "entry", nil, false)
}
