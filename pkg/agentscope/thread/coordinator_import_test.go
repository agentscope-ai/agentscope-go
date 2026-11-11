package thread

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// clearRuleStore holds a cloud conversation and a summary written for the
// edge that covers #1–#6 except an artifact (#4) and a report (#5) the
// cloud never imported. When edgeWrote is set, the edge
// published #8 itself after the summary.
func clearRuleStore(t *testing.T, edgeWrote bool) Store {
	t.Helper()
	s := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, s, nil, userInput("plan the trip"))
	mustAppend(t, s, &Guard{}, ownerChange("", "cloud"))
	cloudReply := func(text string, basedOn uint64) Entry {
		e := replyFrom("cloud", text)
		e.Origin.Path = ""
		e.BasedOn = basedOn
		return e
	}
	mustAppend(t, s, &Guard{Owner: "cloud", Epoch: 1}, cloudReply("here is the plan", 1))
	mustAppend(t, s, nil,
		Entry{Kind: KindArtifact, Content: textBlocks("passport X123"), Refs: []ArtifactRef{{URI: "file:///passport"}}},
		Entry{Kind: KindReport, Content: textBlocks("a shared note")},
	)
	mustAppend(t, s, &Guard{Owner: "cloud", Epoch: 1}, cloudReply("more planning", 0))
	mustAppend(t, s, nil, Entry{
		Kind: KindSummary, Origin: Origin{Participant: "cloud"},
		Summary: &SummaryMeta{Target: "edge", Budget: 200}, Coverage: &Coverage{Through: 6, Excluded: []uint64{4, 5}},
		Content: textBlocks("SUMMARY: the trip plan"),
	})
	if edgeWrote {
		mustAppend(t, s, nil, Entry{Kind: KindArtifact, Origin: Origin{Participant: "edge"},
			Refs: []ArtifactRef{{URI: "file:///photo"}}})
	} else {
		mustAppend(t, s, &Guard{Owner: "cloud", Epoch: 1}, cloudReply("after the summary", 0))
	}
	return s
}

func TestCoordinatorImportClearRule(t *testing.T) {
	store := clearRuleStore(t, false)
	edge, cloud := edgeCloud()
	// The first activation writes nothing, so the edge's last activity stays
	// before the summary's coverage.
	edge.push(func(*fakeRun) *ActivationEnd { return &ActivationEnd{Reason: types.ReplyCompleted} })
	env := newCoEnv(t, store, nil, edge, cloud)
	env.submit(Query{Text: "continue", Explicit: "edge"}) // #9, owner_change #10
	env.idle()

	acts := edge.activations()
	if len(acts) != 1 || acts[0].Trigger.Seq != 9 {
		t.Fatalf("edge activations = %+v", acts)
	}
	s := acts[0].Sync
	if s == nil || !s.Reset || s.PeerSummary == nil || s.PeerSummary.Seq != 7 || s.Cursor != 8 {
		t.Fatalf("sync = %+v", s)
	}
	want := []string{
		"[#4 artifact from coordinator] file:///passport\npassport X123",
		"[#5 report from coordinator] a shared note",
		"[#8 reply from cloud] after the summary",
	}
	if got := syncTexts(s); !reflect.DeepEqual(got, want) {
		t.Fatalf("messages = %q, want %q", got, want)
	}
	env.c.mu.Lock()
	ps := env.c.parts["edge"]
	held, from := ps.heldCoverage, ps.resetFrom
	env.c.mu.Unlock()
	if held == nil || held.Through != 6 || !slices.Equal(held.Excluded, []uint64{4, 5}) || from != 7 {
		t.Fatalf("edge state: held %+v resetFrom %d", held, from)
	}

	// The same summary is not applied twice, even though the edge still has
	// written nothing past its coverage.
	env.say("again")
	env.idle()
	acts = edge.activations()
	if len(acts) != 2 || acts[1].Sync == nil || acts[1].Sync.Reset || !containsText(syncTexts(acts[1].Sync), "foreground: cloud → edge") {
		t.Fatalf("second import = %+v", acts[1].Sync)
	}
}

func TestCoordinatorImportIncremental(t *testing.T) {
	store := clearRuleStore(t, true)
	edge, cloud := edgeCloud()
	env := newCoEnv(t, store, nil, edge, cloud)
	env.submit(Query{Text: "continue", Explicit: "edge"})
	env.idle()
	s := edge.activations()[0].Sync
	if s == nil || s.Reset || s.PeerSummary != nil || s.Cursor != 8 {
		t.Fatalf("sync = %+v", s)
	}
	texts := syncTexts(s)
	if len(texts) != 6 || !containsText(texts, "passport X123") || containsText(texts, "SUMMARY") || containsText(texts, "photo") {
		t.Fatalf("incremental messages = %q", texts)
	}
}

// chunkStore holds n shared artifacts of about 38 tokens each.
func chunkStore(t *testing.T, n int) Store {
	t.Helper()
	s := NewMemoryStore(WithClock(fixedClock()))
	for i := 0; i < n; i++ {
		mustAppend(t, s, nil, Entry{Kind: KindArtifact, Content: textBlocks(strings.Repeat("a", 120))})
	}
	return s
}

// smallEdge has a 400-token window with 150 tokens used: a 50-token budget.
func smallEdge() *fakePart {
	e := newFakePart("edge", ZoneEdge)
	e.desc.Window = 400
	e.state.InputTokens = 150
	return e
}

func allImported(f *fakePart) []string {
	var out []string
	for _, s := range f.appliedSyncs() {
		out = append(out, syncTexts(&s)...)
	}
	return out
}

func TestCoordinatorChunkedImport(t *testing.T) {
	edge := smallEdge()
	_, cloud := edgeCloud()
	env := newCoEnv(t, chunkStore(t, 6), nil, edge, cloud)
	env.submit(Query{Text: "go", Explicit: "edge"}) // #7
	env.idle()
	if n := edge.compactCount(); n < 2 {
		t.Fatalf("%d compactions", n)
	}
	got := allImported(edge)
	if len(got) != 6 {
		t.Fatalf("imported %d messages: %q", len(got), got)
	}
	if s := edge.activations()[0].Sync; s == nil || s.Cursor != 6 {
		t.Fatalf("final sync = %+v", s)
	}
}

func TestCoordinatorChunkedImportStops(t *testing.T) {
	for _, tc := range []struct {
		name     string
		plan     []fakeCompact
		rounds   int
		want     string
		compacts int
	}{
		{"compaction fails twice", []fakeCompact{{ok: false}, {ok: false}}, 0, "[#3–#6 not imported: read them with thread_read]", 2},
		{"compaction error", []fakeCompact{{err: errors.New("model down")}}, 0, "[#2–#6 not imported: read them with thread_read]", 1},
		{"round limit", nil, 1, "[#2–#6 not imported: read them with thread_read]", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edge := smallEdge()
			edge.compactPlan = tc.plan
			_, cloud := edgeCloud()
			env := newCoEnv(t, chunkStore(t, 6), func(cfg *Config) { cfg.Import.MaxRounds = tc.rounds }, edge, cloud)
			env.submit(Query{Text: "go", Explicit: "edge"})
			env.idle()
			s := edge.activations()[0].Sync
			if s == nil || s.Cursor != 6 || !reflect.DeepEqual(syncTexts(s), []string{tc.want}) {
				t.Fatalf("final sync = %+v %q", s, syncTexts(s))
			}
			if n := edge.compactCount(); n != tc.compacts {
				t.Fatalf("%d compactions, want %d", n, tc.compacts)
			}
		})
	}
}

func TestCoordinatorChunkedImportSkipsOneEntry(t *testing.T) {
	edge := smallEdge()
	edge.compactPlan = []fakeCompact{{err: errors.New("down")}}
	_, cloud := edgeCloud()
	env := newCoEnv(t, chunkStore(t, 2), nil, edge, cloud)
	env.submit(Query{Text: "go", Explicit: "edge"})
	env.idle()
	s := edge.activations()[0].Sync
	if got := syncTexts(s); !reflect.DeepEqual(got, []string{"[#2 not imported: read it with thread_read]"}) {
		t.Fatalf("final sync = %q", got)
	}
	if k := s.Messages[0].Metadata[KindMetadataKey]; k != string(KindMarker) {
		t.Fatalf("the notice's kind = %v", k)
	}
}

func TestCoordinatorImportSyncErrorFailsOver(t *testing.T) {
	edge := smallEdge()
	edge.syncErr = errors.New("context locked")
	_, cloud := edgeCloud()
	env := newCoEnv(t, chunkStore(t, 4), nil, edge, cloud)
	env.submit(Query{Text: "go", Explicit: "edge"})
	env.idle()
	if n := len(edge.activations()); n != 0 {
		t.Fatalf("edge ran %d times", n)
	}
	if n := len(cloud.activations()); n != 1 {
		t.Fatalf("cloud ran %d times", n)
	}
}

func TestCoordinatorPendingBudget(t *testing.T) {
	edge := smallEdge()
	edge.started = make(chan Activation, 8)
	_, cloud := edgeCloud()
	release := make(chan struct{})
	var first, second *Sync
	edge.push(gate(release, func(r *fakeRun) *ActivationEnd {
		first = r.pending()
		second = r.pending()
		return r.reply("ok")
	}))
	env := newCoEnv(t, nil, nil, edge, cloud)
	env.say("start")
	waitStarted(t, edge)
	long := strings.Repeat("b", 120)
	a := env.say(long + " one")
	b := env.say(long + " two")
	c := env.say(long + " three")
	close(release)
	env.idle()
	if first == nil || first.Cursor != a || !containsText(syncTexts(first), "one") || containsText(syncTexts(first), "two") {
		t.Fatalf("first pending = %+v %q", first, syncTexts(first))
	}
	if second == nil || second.Cursor >= c || second.Cursor < b {
		t.Fatalf("second pending = %+v", second)
	}
	// What Pending could not deliver started a new cycle.
	acts := edge.activations()
	if len(acts) != 2 || acts[1].Trigger.Seq != c {
		t.Fatalf("activations = %+v", acts)
	}
}
