package thread

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

var testTime = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

func fixedClock() func() time.Time { return func() time.Time { return testTime } }

func textBlocks(s string) []message.ContentBlock {
	return []message.ContentBlock{message.TextBlock{Type: "text", Text: s}}
}

func userInput(s string) Entry {
	return Entry{Kind: KindUserInput, Content: textBlocks(s)}
}

func replyFrom(participant, s string) Entry {
	return Entry{
		Kind:    KindReply,
		Origin:  Origin{Participant: participant, Path: "master"},
		Content: textBlocks(s),
	}
}

func ownerChange(from, to string) Entry {
	return Entry{
		Kind:  KindOwnerChange,
		Owner: &OwnerChange{From: from, To: to, Reason: ReasonRouter},
	}
}

func report(participant, s string) Entry {
	return Entry{
		Kind:    KindReport,
		Origin:  Origin{Participant: participant},
		Content: textBlocks(s),
	}
}

func withKey(e Entry, key string) Entry { //nolint:gocritic // test helper
	e.DedupKey = key
	return e
}

type storeFactory struct {
	name string
	open func(t *testing.T, opts ...StoreOption) Store
}

func storeFactories() []storeFactory {
	return []storeFactory{
		{name: "memory", open: func(_ *testing.T, opts ...StoreOption) Store {
			return NewMemoryStore(opts...)
		}},
		{name: "file", open: func(t *testing.T, opts ...StoreOption) Store {
			t.Helper()
			s, err := OpenFileStore(filepath.Join(t.TempDir(), "thread.jsonl"), append([]StoreOption{WithoutSync()}, opts...)...)
			if err != nil {
				t.Fatalf("OpenFileStore: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		}},
	}
}

// eachStore runs fn against a fresh store of every implementation.
func eachStore(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Helper()
	for _, f := range storeFactories() {
		t.Run(f.name, func(t *testing.T) {
			fn(t, f.open(t, WithClock(fixedClock())))
		})
	}
}

func mustAppend(t *testing.T, s Store, g *Guard, entries ...Entry) uint64 {
	t.Helper()
	seq, err := s.Append(context.Background(), entries, g)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return seq
}

func mustHead(t *testing.T, s Store) Head {
	t.Helper()
	h, err := s.Head(context.Background())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	return h
}

func mustRead(t *testing.T, s Store, after uint64, f Filter, limit int) []Entry {
	t.Helper()
	got, err := s.Read(context.Background(), after, f, limit)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return got
}

func seqs(entries []Entry) []uint64 {
	out := make([]uint64, 0, len(entries))
	for i := range entries {
		out = append(out, entries[i].Seq)
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
