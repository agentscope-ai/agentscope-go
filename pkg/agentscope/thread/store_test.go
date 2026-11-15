package thread

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

func TestStoreAppendAssignsSeqTimeAndEpoch(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		own := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
		a := userInput("first")
		a.Seq = 99
		b := report("edge", "second")
		b.Time = own
		if seq := mustAppend(t, s, nil, a, b); seq != 2 {
			t.Fatalf("Append returned %d, want 2", seq)
		}
		got := mustRead(t, s, 0, Filter{}, 0)
		if !reflect.DeepEqual(seqs(got), []uint64{1, 2}) {
			t.Fatalf("seqs %v, want [1 2]", seqs(got))
		}
		if !got[0].Time.Equal(testTime) {
			t.Errorf("zero time set to %v, want the clock's %v", got[0].Time, testTime)
		}
		if !got[1].Time.Equal(own) {
			t.Errorf("caller time changed to %v, want %v", got[1].Time, own)
		}
		for i := range got {
			if got[i].Epoch != 0 {
				t.Errorf("entry %d epoch %d before any owner change, want 0", got[i].Seq, got[i].Epoch)
			}
		}
		if h := mustHead(t, s); h != (Head{LastSeq: 2}) {
			t.Errorf("head %+v, want LastSeq 2 and no owner", h)
		}
	})
}

func TestStoreAppendValidation(t *testing.T) {
	cases := []struct {
		name  string
		batch []Entry
		want  string
	}{
		{"empty batch", nil, "at least one entry"},
		{"unknown kind", []Entry{{Kind: "chat"}}, "unknown entry kind"},
		{"tool call content", []Entry{{Kind: KindReply, Content: []message.ContentBlock{message.ToolCallBlock{Name: "Read"}}}}, "may not hold"},
		{"tool result content", []Entry{{Kind: KindReport, Content: []message.ContentBlock{&message.ToolResultBlock{Name: "Read"}}}}, "may not hold"},
		{"thinking content", []Entry{{Kind: KindReport, Content: []message.ContentBlock{message.ThinkingBlock{Thinking: "hm"}}}}, "may not hold"},
		{"redacted thinking content", []Entry{{Kind: KindReport, Content: []message.ContentBlock{message.RedactedThinkingBlock{Data: "x"}}}}, "may not hold"},
		{"nil block", []Entry{{Kind: KindReport, Content: []message.ContentBlock{nil}}}, "nil content block"},
		{"nil text pointer", []Entry{{Kind: KindReport, Content: []message.ContentBlock{(*message.TextBlock)(nil)}}}, "nil text block"},
		{"nil data pointer", []Entry{{Kind: KindReport, Content: []message.ContentBlock{(*message.DataBlock)(nil)}}}, "nil data block"},
		{"nil hint pointer", []Entry{{Kind: KindReport, Content: []message.ContentBlock{(*message.HintBlock)(nil)}}}, "nil hint block"},
		{"tool call inside a hint", []Entry{{Kind: KindReport, Content: []message.ContentBlock{
			message.HintBlock{Hint: []message.ContentBlock{message.ToolCallBlock{Name: "Bash"}}},
		}}}, "may not hold"},
		{"hint of another type", []Entry{{Kind: KindReport, Content: []message.ContentBlock{message.HintBlock{Hint: 3}}}}, "hint int cannot be stored"},
		{"nil source pointer", []Entry{{Kind: KindReport, Content: []message.ContentBlock{message.DataBlock{Source: (*message.URLSource)(nil)}}}}, "cannot be stored"},
		{"data without source", []Entry{{Kind: KindReport, Content: []message.ContentBlock{message.DataBlock{}}}}, "cannot be stored"},
		{"data with string source", []Entry{{Kind: KindReport, Content: []message.ContentBlock{message.DataBlock{Source: "x"}}}}, "cannot be stored"},
		{"owner change without owner", []Entry{{Kind: KindOwnerChange}}, "owner_change needs"},
		{"owner change without target", []Entry{{Kind: KindOwnerChange, Owner: &OwnerChange{From: "edge"}}}, "owner_change needs"},
		{"summary without coverage", []Entry{{Kind: KindSummary, Summary: &SummaryMeta{Target: "edge"}}}, "needs a coverage"},
		{"progress without run", []Entry{{Kind: KindProgress, Content: textBlocks("1. [in_progress] x")}}, "needs the run"},
		{"summary without meta", []Entry{{Kind: KindSummary, Coverage: &Coverage{Through: 1}}}, "needs a target"},
		{"summary without target", []Entry{{Kind: KindSummary, Coverage: &Coverage{Through: 1}, Summary: &SummaryMeta{}}}, "needs a target"},
		{"assign without task", []Entry{{Kind: KindAssign}}, "assign needs"},
		{"assign without id", []Entry{{Kind: KindAssign, Task: &Task{}}}, "assign needs"},
		{"assign with an end", []Entry{{Kind: KindAssign, Task: &Task{ID: "t-1", End: types.ReplyCompleted}}}, "may not carry a task end"},
		{"marker without payload", []Entry{{Kind: KindMarker}}, "marker needs"},
		{"invalid second entry", []Entry{report("edge", "ok"), {Kind: KindMarker}}, "batch entry 1"},
		{"key repeated in batch", []Entry{withKey(report("edge", "a"), "k"), withKey(report("edge", "b"), "k")}, "repeats within the batch"},
	}
	eachStore(t, func(t *testing.T, s Store) {
		mustAppend(t, s, nil, userInput("before"))
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := s.Append(context.Background(), tc.batch, &Guard{})
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("Append error %v, want one containing %q", err, tc.want)
				}
				if errors.Is(err, ErrDuplicate) || errors.Is(err, ErrNotOwner) {
					t.Errorf("validation error %v wraps a sentinel", err)
				}
				if h := mustHead(t, s); h.LastSeq != 1 {
					t.Errorf("a rejected batch wrote entries: head %+v", h)
				}
			})
		}
	})
}

func TestStoreNormalizesContentBlocks(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		e := report("edge", "")
		e.Content = []message.ContentBlock{
			&message.TextBlock{Text: "pointer text"},
			message.DataBlock{Source: &message.URLSource{URL: "https://example.com/a.png", MediaType: "image/png"}},
			&message.DataBlock{Source: &message.Base64Source{Data: "aGk=", MediaType: "image/png"}},
			message.HintBlock{Hint: []message.ContentBlock{&message.TextBlock{Text: "inner"}}},
			&message.HintBlock{Hint: "plain hint"},
			message.HintBlock{},
		}
		mustAppend(t, s, nil, e)
		got, _, _ := s.Get(context.Background(), 1)
		want := []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "pointer text"},
			message.DataBlock{Type: "data", Source: message.URLSource{Type: "url", URL: "https://example.com/a.png", MediaType: "image/png"}},
			message.DataBlock{Type: "data", Source: message.Base64Source{Type: "base64", Data: "aGk=", MediaType: "image/png"}},
			message.HintBlock{Type: "hint", Hint: []message.ContentBlock{message.TextBlock{Type: "text", Text: "inner"}}},
			message.HintBlock{Type: "hint", Hint: "plain hint"},
			message.HintBlock{Type: "hint", Hint: ""},
		}
		if !reflect.DeepEqual(got.Content, want) {
			t.Fatalf("content %#v, want %#v", got.Content, want)
		}
	})
}

func TestStoreGuard(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		// No guard for a reply or an owner change.
		for _, e := range []Entry{replyFrom("edge", "hi"), ownerChange("", "edge")} {
			_, err := s.Append(ctx, []Entry{e}, nil)
			if !errors.Is(err, ErrNotOwner) || !strings.Contains(err.Error(), "needs a guard") {
				t.Errorf("%s without a guard: %v, want ErrNotOwner with a reason", e.Kind, err)
			}
		}
		// A batch mixing a report with a reply still needs the guard.
		if _, err := s.Append(ctx, []Entry{report("edge", "r"), replyFrom("edge", "hi")}, nil); !errors.Is(err, ErrNotOwner) {
			t.Errorf("mixed batch without a guard: %v, want ErrNotOwner", err)
		}
		// Before any owner change the zero guard is valid.
		mustAppend(t, s, &Guard{}, ownerChange("", "edge"))
		if h := mustHead(t, s); h.Owner != "edge" || h.Epoch != 1 {
			t.Fatalf("head %+v, want edge at epoch 1", h)
		}
		mismatches := []Guard{{Owner: "", Epoch: 0}, {Owner: "cloud", Epoch: 1}, {Owner: "edge", Epoch: 2}}
		for _, g := range mismatches {
			_, err := s.Append(ctx, []Entry{replyFrom("edge", "hi")}, &g)
			if !errors.Is(err, ErrNotOwner) || !strings.Contains(err.Error(), `the log has "edge" at epoch 1`) {
				t.Errorf("guard %+v: %v, want ErrNotOwner naming the head", g, err)
			}
		}
		// A guard on an unguarded kind must still match.
		if _, err := s.Append(ctx, []Entry{report("edge", "r")}, &Guard{Owner: "cloud", Epoch: 1}); !errors.Is(err, ErrNotOwner) {
			t.Errorf("stale guard on a report: %v, want ErrNotOwner", err)
		}
		mustAppend(t, s, &Guard{Owner: "edge", Epoch: 1}, replyFrom("edge", "hi"))
		// Background kinds never need a guard, whatever the epoch.
		background := []Entry{
			userInput("u"),
			report("cloud", "r"),
			{Kind: KindAssign, Task: &Task{ID: "t-1"}, To: []string{"cloud"}},
			{Kind: KindArtifact, Refs: []ArtifactRef{{URI: "file:///a"}}},
			{Kind: KindSummary, Coverage: &Coverage{Through: 2}, Summary: &SummaryMeta{Target: "edge"}},
			{Kind: KindMarker, Marker: &Marker{Kind: MarkerRecovered}},
		}
		for _, e := range background {
			if _, err := s.Append(ctx, []Entry{e}, nil); err != nil {
				t.Errorf("unguarded %s: %v", e.Kind, err)
			}
		}
		if h := mustHead(t, s); h.LastSeq != 8 || h.Epoch != 1 {
			t.Errorf("head %+v, want LastSeq 8 at epoch 1", h)
		}
	})
}

func TestStoreEpochAcrossBatch(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		last := mustAppend(t, s, &Guard{},
			replyFrom("edge", "before"),
			ownerChange("", "cloud"),
			replyFrom("cloud", "middle"),
			ownerChange("cloud", "edge"),
			report("cloud", "after"),
		)
		if last != 5 {
			t.Fatalf("Append returned %d, want 5", last)
		}
		got := mustRead(t, s, 0, Filter{}, 0)
		var epochs []uint64
		for i := range got {
			epochs = append(epochs, got[i].Epoch)
		}
		if !reflect.DeepEqual(epochs, []uint64{0, 1, 1, 2, 2}) {
			t.Fatalf("epochs %v, want [0 1 1 2 2]", epochs)
		}
		if h := mustHead(t, s); h != (Head{LastSeq: 5, Owner: "edge", Epoch: 2}) {
			t.Fatalf("head %+v, want edge at epoch 2", h)
		}
		if _, err := s.Append(context.Background(), []Entry{replyFrom("cloud", "late")}, &Guard{Owner: "cloud", Epoch: 1}); !errors.Is(err, ErrNotOwner) {
			t.Errorf("guard from the middle of the batch: %v, want ErrNotOwner", err)
		}
		if seq := mustAppend(t, s, &Guard{Owner: "edge", Epoch: 2}, replyFrom("edge", "now")); seq != 6 {
			t.Errorf("Append returned %d, want 6", seq)
		}
	})
}

func TestStoreDedup(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		first := []Entry{withKey(ownerChange("", "edge"), "k1"), withKey(replyFrom("edge", "hi"), "k2")}
		if seq := mustAppend(t, s, &Guard{}, first...); seq != 2 {
			t.Fatalf("Append returned %d, want 2", seq)
		}
		mustAppend(t, s, nil, withKey(report("edge", "other"), "k3"))

		// The acknowledgement was lost: the same batch with its old guard
		// comes back after the epoch moved.
		seq, err := s.Append(ctx, first, &Guard{})
		if err != nil || seq != 2 {
			t.Fatalf("retry returned %d, %v; want 2, nil", seq, err)
		}
		if seq, err := s.Append(ctx, []Entry{withKey(report("edge", "x"), "k3")}, nil); err != nil || seq != 3 {
			t.Errorf("single-entry retry returned %d, %v; want 3, nil", seq, err)
		}
		if h := mustHead(t, s); h.LastSeq != 3 {
			t.Fatalf("retries wrote entries: head %+v", h)
		}

		partial := [][]Entry{
			{withKey(report("edge", "a"), "k2"), withKey(report("edge", "b"), "k4")},
			{withKey(report("edge", "a"), "k1"), report("edge", "unkeyed")},
		}
		for i, batch := range partial {
			if _, err := s.Append(ctx, batch, nil); !errors.Is(err, ErrDuplicate) {
				t.Errorf("partial duplicate %d: %v, want ErrDuplicate", i, err)
			}
		}
		if h := mustHead(t, s); h.LastSeq != 3 {
			t.Fatalf("partial duplicates wrote entries: head %+v", h)
		}
		if seq := mustAppend(t, s, nil, withKey(report("edge", "new"), "k4"), report("edge", "unkeyed")); seq != 5 {
			t.Errorf("fresh keys returned %d, want 5", seq)
		}
	})
}

func TestStoreConcurrentAppends(t *testing.T) {
	const workers, perWorker = 16, 40
	eachStore(t, func(t *testing.T, s Store) {
		var wg sync.WaitGroup
		errs := make(chan error, workers*perWorker)
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range perWorker {
					batch := []Entry{report(fmt.Sprintf("p%d", w), fmt.Sprint(i))}
					if i%3 == 0 {
						batch = append(batch, withKey(report("x", "keyed"), fmt.Sprintf("w%d-%d", w, i)))
					}
					if _, err := s.Append(context.Background(), batch, nil); err != nil {
						errs <- err
					}
					if _, err := s.Read(context.Background(), 0, Filter{Kinds: []Kind{KindReport}}, 5); err != nil {
						errs <- err
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent call: %v", err)
		}
		got := mustRead(t, s, 0, Filter{}, 0)
		want := workers*perWorker + workers*((perWorker+2)/3)
		if len(got) != want {
			t.Fatalf("%d entries, want %d", len(got), want)
		}
		for i := range got {
			if got[i].Seq != uint64(i+1) {
				t.Fatalf("entry %d has seq %d: seqs are not contiguous", i, got[i].Seq)
			}
		}
		if h := mustHead(t, s); h.LastSeq != uint64(want) {
			t.Errorf("head %+v, want LastSeq %d", h, want)
		}
	})
}

func TestStoreReadFilters(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		toCloud := report("edge", "for cloud")
		toCloud.To = []string{"cloud"}
		assign := Entry{Kind: KindAssign, Origin: Origin{Participant: "cloud"}, Task: &Task{ID: "t"}, To: []string{"edge", "cloud"}}
		mustAppend(t, s, &Guard{},
			userInput("hello"),        // 1
			replyFrom("cloud", "hi"),  // 2
			report("edge", "found"),   // 3
			toCloud,                   // 4
			assign,                    // 5
			ownerChange("", "cloud"),  // 6
			replyFrom("cloud", "bye"), // 7
		)
		cases := []struct {
			name  string
			after uint64
			f     Filter
			limit int
			want  []uint64
		}{
			{"all", 0, Filter{}, 0, []uint64{1, 2, 3, 4, 5, 6, 7}},
			{"negative limit", 0, Filter{}, -1, []uint64{1, 2, 3, 4, 5, 6, 7}},
			{"after", 4, Filter{}, 0, []uint64{5, 6, 7}},
			{"after the end", 7, Filter{}, 0, nil},
			{"far after the end", 1000, Filter{}, 0, nil},
			{"limit", 1, Filter{}, 2, []uint64{2, 3}},
			{"kinds", 0, Filter{Kinds: []Kind{KindReply, KindOwnerChange}}, 0, []uint64{2, 6, 7}},
			{"to", 0, Filter{To: "cloud"}, 0, []uint64{4, 5}},
			{"exclude origin", 0, Filter{ExcludeOrigin: "cloud"}, 0, []uint64{1, 3, 4, 6}},
			{"combined with limit", 2, Filter{Kinds: []Kind{KindAssign, KindOwnerChange, KindReply}, ExcludeOrigin: "edge"}, 2, []uint64{5, 6}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := seqs(mustRead(t, s, tc.after, tc.f, tc.limit))
				if len(got) == 0 && len(tc.want) == 0 {
					return
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("seqs %v, want %v", got, tc.want)
				}
			})
		}
	})
}

func TestStoreGet(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		mustAppend(t, s, nil, userInput("one"))
		for _, seq := range []uint64{0, 2} {
			if _, ok, err := s.Get(context.Background(), seq); ok || err != nil {
				t.Errorf("Get(%d) = %v, %v; want false, nil", seq, ok, err)
			}
		}
		e, ok, err := s.Get(context.Background(), 1)
		if !ok || err != nil || e.Text() != "one" {
			t.Errorf("Get(1) = %q, %v, %v", e.Text(), ok, err)
		}
	})
}

func richEntry() Entry {
	return Entry{
		Kind:   KindOwnerChange,
		Origin: Origin{Participant: "edge", Path: "master"},
		To:     []string{"cloud"},
		Content: []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "hand over"},
			message.DataBlock{Type: "data", Source: map[string]any{"raw": "x"}},
			message.HintBlock{Type: "hint", Hint: []message.ContentBlock{message.TextBlock{Type: "text", Text: "inner"}}},
		},
		Digest:   &ToolDigest{Calls: []ToolCallDigest{{Name: "Read", Args: `{"path":"a.go"}`}}},
		Refs:     []ArtifactRef{{URI: "file:///a", Summary: "a", Size: 3}},
		Coverage: &Coverage{Through: 4, Excluded: []uint64{2}},
		Summary:  &SummaryMeta{Target: "edge"},
		Owner: &OwnerChange{From: "edge", To: "cloud", Reason: ReasonTransfer, Brief: &Brief{
			Goal: "g", Done: []string{"d"}, Remaining: []string{"r"}, Constraints: []string{"c"},
			Refs: []ArtifactRef{{URI: "file:///b"}},
		}},
		Task:   &Task{ID: "t-1"},
		Marker: &Marker{Kind: MarkerRestarted, Detail: "d"},
	}
}

func mutateEntry(e *Entry) {
	e.To[0] = "mutated"
	e.Content[0] = message.TextBlock{Type: "text", Text: "mutated"}
	if d, ok := e.Content[1].(message.DataBlock); ok {
		if m, ok := d.Source.(map[string]any); ok {
			m["raw"] = "mutated"
		}
	}
	if h, ok := e.Content[2].(message.HintBlock); ok {
		if inner, ok := h.Hint.([]message.ContentBlock); ok {
			inner[0] = message.TextBlock{Text: "mutated"}
		}
	}
	e.Digest.Calls[0].Name = "mutated"
	e.Refs[0].URI = "mutated"
	e.Coverage.Excluded[0] = 99
	e.Coverage.Through = 99
	e.Summary.Target = "mutated"
	e.Owner.To = "mutated"
	e.Owner.Brief.Goal = "mutated"
	e.Owner.Brief.Done[0] = "mutated"
	e.Owner.Brief.Remaining[0] = "mutated"
	e.Owner.Brief.Constraints[0] = "mutated"
	e.Owner.Brief.Refs[0].URI = "mutated"
	e.Task.ID = "mutated"
	e.Marker.Detail = "mutated"
}

func TestStoreDeepCopies(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		in := richEntry()
		mustAppend(t, s, &Guard{}, in)
		stored, _, _ := s.Get(ctx, 1)
		want := mustJSON(t, stored)

		mutateEntry(&in)
		read := mustRead(t, s, 0, Filter{}, 0)
		mutateEntry(&read[0])
		got, _, _ := s.Get(ctx, 1)
		mutateEntry(&got)

		again, _, _ := s.Get(ctx, 1)
		if g := mustJSON(t, again); g != want {
			t.Fatalf("stored entry changed through a copy:\n got %s\nwant %s", g, want)
		}
		if h := mustHead(t, s); h.Owner != "cloud" {
			t.Errorf("head owner %q, want cloud", h.Owner)
		}
	})
}

func TestStoreClosed(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		mustAppend(t, s, nil, userInput("x"))
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, err := s.Append(ctx, []Entry{userInput("y")}, nil); !errors.Is(err, ErrClosed) {
			t.Errorf("Append after Close: %v", err)
		}
		if _, err := s.Read(ctx, 0, Filter{}, 0); !errors.Is(err, ErrClosed) {
			t.Errorf("Read after Close: %v", err)
		}
		if _, _, err := s.Get(ctx, 1); !errors.Is(err, ErrClosed) {
			t.Errorf("Get after Close: %v", err)
		}
		if _, err := s.Head(ctx); !errors.Is(err, ErrClosed) {
			t.Errorf("Head after Close: %v", err)
		}
		if err := s.Close(); !errors.Is(err, ErrClosed) {
			t.Errorf("second Close: %v", err)
		}
	})
}

func TestStoreCanceledContext(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := s.Append(ctx, []Entry{userInput("y")}, nil); !errors.Is(err, context.Canceled) {
			t.Errorf("Append: %v", err)
		}
		if _, err := s.Read(ctx, 0, Filter{}, 0); !errors.Is(err, context.Canceled) {
			t.Errorf("Read: %v", err)
		}
		if _, _, err := s.Get(ctx, 1); !errors.Is(err, context.Canceled) {
			t.Errorf("Get: %v", err)
		}
		if _, err := s.Head(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("Head: %v", err)
		}
		if h := mustHead(t, s); h.LastSeq != 0 {
			t.Errorf("canceled Append wrote: %+v", h)
		}
	})
}

func TestStoreOptions(t *testing.T) {
	s := NewMemoryStore(nil, WithClock(nil))
	mustAppend(t, s, nil, userInput("x"))
	e, _, _ := s.Get(context.Background(), 1)
	if e.Time.IsZero() {
		t.Fatal("a nil clock left the time unset")
	}
	if e.Time.After(time.Now().Add(time.Minute)) {
		t.Fatalf("time %v is in the future", e.Time)
	}
}
