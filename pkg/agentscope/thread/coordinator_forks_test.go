package thread

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

func forkEnv(t *testing.T, store Store, mutate func(*Config)) (*coEnv, *fakePart, *fakeForker) {
	t.Helper()
	edge := newFakePart("edge", ZoneEdge)
	cloud := newFakeForker("cloud")
	env := newCoEnv(t, store, func(cfg *Config) {
		cfg.Router = cloudRouter()
		cfg.Summaries.PeerEvery = 100
		if mutate != nil {
			mutate(cfg)
		}
	}, edge, cloud)
	return env, edge, cloud
}

// grow replies text after the participant's input reached tokens.
func grow(tokens int, text string) fakeScript {
	return func(r *fakeRun) *ActivationEnd {
		r.f.setState(func(s *ParticipantState) { s.InputTokens = tokens })
		return r.reply(text)
	}
}

func TestCoordinatorPeerForkAfterGrowth(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, Entry{Kind: KindArtifact, Refs: []ArtifactRef{{URI: "file:///diary"}}})
	env, _, cloud := forkEnv(t, store, nil)
	cloud.push(grow(500, "plan ready"), grow(700, "more planning"))
	env.say("plan") // #2, owner_change #3, reply #4
	env.idle()

	sums := findEntries(env.log(), KindSummary, "")
	if len(sums) != 1 {
		t.Fatalf("summaries = %+v", sums)
	}
	s := sums[0]
	if s.Origin.Participant != "cloud" || s.Summary.Target != "edge" || s.Summary.Budget != 200 || s.Summary.Lossy ||
		s.Text() != "peer summary text" || !strings.HasPrefix(s.DedupKey, "peer:") {
		t.Fatalf("summary = %+v %+v", s, s.Summary)
	}
	if s.Coverage.Through != 4 || !slices.Equal(s.Coverage.Excluded, []uint64{3}) {
		t.Fatalf("coverage = %+v", s.Coverage)
	}
	specs := cloud.forkSpecs()
	if len(specs) != 1 {
		t.Fatalf("%d forks", len(specs))
	}
	spec := specs[0]
	if !strings.Contains(spec.Prompt, "for edge,") || !strings.Contains(spec.Prompt, "8000-token") ||
		!strings.Contains(spec.Prompt, "under 200 tokens") || spec.MaxTokens != 512 || spec.MaxRewrites != 1 || spec.Appendix != "" {
		t.Fatalf("spec = %+v", spec)
	}
	if fb, ok := spec.Validate("short"); !ok || fb != "" {
		t.Fatalf("Validate(short) = %q %v", fb, ok)
	}
	if fb, ok := spec.Validate(strings.Repeat("x", 1204)); ok || fb != "Too long (301 tokens). Rewrite it under 200 tokens." {
		t.Fatalf("Validate(long) = %q %v", fb, ok)
	}
	var sawSummary bool
	for _, ev := range env.events() {
		sawSummary = sawSummary || (ev.Kind == EventEntry && ev.Entry.Kind == KindSummary)
	}
	if !sawSummary {
		t.Fatal("the summary was not shown")
	}

	env.say("more")
	env.idle()
	specs = cloud.forkSpecs()
	if len(specs) != 2 || specs[1].Appendix != "Previous summary for edge, update it:\npeer summary text" {
		t.Fatalf("second fork = %+v", specs)
	}
	tot := env.c.Ledger().TotalsByKind()[UsageForkPeer]
	if tot.Calls != 2 || tot.InputTokens != 20 || tot.UnknownCalls != 0 {
		t.Fatalf("fork ledger = %+v", tot)
	}
}

// A peer fork needs output sealed past the newest summary's coverage: growth
// from an activation that sealed nothing buys no new summary, and a
// background task's report counts as sealed output.
func TestCoordinatorPeerForkNeedsNewSealedOutput(t *testing.T) {
	env, edge, cloud := forkEnv(t, nil, func(cfg *Config) {
		cfg.Router = &fakeRouter{fn: func(_ context.Context, req *RouteRequest) (RouteDecision, error) {
			if strings.HasPrefix(req.Query, "edge:") {
				return RouteDecision{Target: "edge", Source: RouteRule}, nil
			}
			return RouteDecision{Target: "cloud", Source: RouteRule}, nil
		}}
	})
	cloud.push(grow(500, "plan ready"), grow(900, ""), grow(1500, "it opens at ten"))
	env.say("plan")
	env.idle()
	if n := len(cloud.forkSpecs()); n != 1 {
		t.Fatalf("forks after the first reply = %d", n)
	}

	env.say("again") // grows past PeerEvery, seals nothing
	env.idle()
	if n := len(cloud.forkSpecs()); n != 1 {
		t.Fatalf("an activation that sealed nothing forked again: %d forks", n)
	}

	edge.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("cloud", "look up the opening hours", false); err != nil {
			t.Errorf("assign: %v", err)
		}
		return r.reply("asked the cloud")
	})
	env.say("edge: ask the cloud")
	env.idle()
	reps := findEntries(env.log(), KindReport, "")
	if len(reps) != 1 || reps[0].Origin.Participant != "cloud" {
		t.Fatalf("reports = %+v", reps)
	}
	sums := findEntries(env.log(), KindSummary, "")
	if len(sums) != 2 || sums[1].Coverage.Through != reps[0].Seq {
		t.Fatalf("summaries = %+v, want the second through the report #%d", sums, reps[0].Seq)
	}

	// The edge kept the foreground while the cloud's task ran: the summary
	// that task produced does not reset it, and the edge receives the
	// task's answer itself.
	edge.push(replyWith("it opens at ten, then"))
	env.say("edge: what did the cloud find?")
	env.idle()
	acts := edge.activations()
	last := acts[len(acts)-1].Sync
	if last == nil || last.Reset || last.PeerSummary != nil || !containsText(syncTexts(last), "it opens at ten") {
		t.Fatalf("the edge that kept the foreground was reset or missed the report: %+v %q", last, syncTexts(last))
	}
}

// ownEnv is forkEnv with the own notes scheduled at small sizes and the
// peer summary rules off, so peer summaries come only with own notes.
func ownEnv(t *testing.T) (*coEnv, *fakeForker) {
	t.Helper()
	env, _, cloud := forkEnv(t, nil, func(cfg *Config) {
		cfg.Summaries.PeerEvery = 1 << 30
		cfg.Summaries.PeerRatio = -1
		cfg.Summaries.OwnInit = 600
		cfg.Summaries.OwnEvery = 300
	})
	return env, cloud
}

// TestCoordinatorOwnNotesFollowClaudeCode: the own notes start once the
// input reaches OwnInit, then update whenever it grew OwnEvery, as Claude
// Code's session memory does. An update sends the notes and gets edits
// back; the edited notes are offered, not applied. A compaction (the input
// dropping) lowers the baseline.
func TestCoordinatorOwnNotesFollowClaudeCode(t *testing.T) {
	env, cloud := ownEnv(t)
	cloud.push(grow(500, "a"), grow(700, "b"), grow(800, "c"), grow(1000, "d"), grow(400, "e"), grow(650, "f"), grow(700, "g"))
	ownSpecs := func() []ForkSpec {
		var out []ForkSpec
		for _, sp := range cloud.forkSpecs() {
			if sp.Prompt == DefaultOwnPrompt || sp.Prompt == DefaultOwnUpdatePrompt {
				out = append(out, sp)
			}
		}
		return out
	}
	step := func(q string, wantOwn int) {
		t.Helper()
		env.say(q)
		env.idle()
		if n := len(ownSpecs()); n != wantOwn {
			t.Fatalf("after %q: %d own forks, want %d", q, n, wantOwn)
		}
	}
	step("a", 0) // 500: below OwnInit
	step("b", 1) // 700: the first notes
	first := ownSpecs()[0]
	if first.Prompt != DefaultOwnPrompt || first.Appendix != "" || first.Validate == nil || first.MaxTokens != defaultOwnMaxTokens {
		t.Fatalf("first own spec = %+v", first)
	}
	step("c", 1) // 800: grew 100
	cloud.fmu.Lock()
	cloud.ownOut = ForkOutput{Text: editFind + "\nown summary text\n" + editSep + "\nown notes v2\n" + editReplace, Calls: 1}
	cloud.fmu.Unlock()
	step("d", 2) // 1000: grew 300 since the first notes
	upd := ownSpecs()[1]
	if upd.Prompt != DefaultOwnUpdatePrompt || upd.Appendix != "Your notes so far:\nown summary text" {
		t.Fatalf("update spec = %+v", upd)
	}
	if fb, ok := upd.Validate("not an edit of anything\n" + editFind + "\nmissing\n" + editSep + "\nx\n" + editReplace); ok || !strings.Contains(fb, "edit 1") {
		t.Fatalf("the update validator accepted an edit that does not apply: %q", fb)
	}
	if got := cloud.offeredOwn(); len(got) != 2 || got[0] != "own summary text" || got[1] != "own notes v2" {
		t.Fatalf("offered = %q", got)
	}
	step("e", 2) // 400: compacted; the baseline drops to 400
	step("f", 2) // 650: grew 250
	cloud.fmu.Lock()
	cloud.ownOut = ForkOutput{Text: editFind + "\n" + editSep + "\nnext: book the hotel\n" + editReplace, Calls: 1}
	cloud.fmu.Unlock()
	step("g", 3) // 700: grew 300 since the compaction
	if got := cloud.offeredOwn(); len(got) != 3 || got[2] != "own notes v2\nnext: book the hotel" {
		t.Fatalf("offered = %q", got)
	}
	if tot := env.c.Ledger().TotalsByKind()[UsageForkOwn]; tot.Calls != 3 {
		t.Fatalf("own fork ledger = %+v", tot)
	}
}

// TestCoordinatorOwnNotesFailures: a failed own fork and an update whose
// edits do not apply leave the notes as they were; notes over OwnMaxTokens
// are middle-cut; an offer the participant refuses still leaves the notes as
// the base of the next update.
func TestCoordinatorOwnNotesFailures(t *testing.T) {
	env, _, cloud := forkEnv(t, nil, func(cfg *Config) {
		cfg.Summaries.PeerEvery = 1 << 30
		cfg.Summaries.PeerRatio = -1
		cfg.Summaries.OwnInit = 600
		cfg.Summaries.OwnEvery = 300
		cfg.Summaries.OwnMaxTokens = 50
	})
	cloud.push(grow(700, "a"), grow(1000, "b"), grow(1300, "c"), grow(1600, "d"), grow(1900, "e"), grow(2200, "f"),
		grow(2500, "g"), grow(2800, "h"))
	notes := func() string {
		env.c.mu.Lock()
		defer env.c.mu.Unlock()
		return env.c.parts["cloud"].ownNotes
	}
	step := func(q string, set func()) {
		t.Helper()
		cloud.fmu.Lock()
		set()
		cloud.fmu.Unlock()
		env.say(q)
		env.idle()
	}
	step("a", func() { cloud.forkErr = errors.New("rate limited") })
	if n, got := notes(), cloud.offeredOwn(); n != "" || len(got) != 0 {
		t.Fatalf("after a failed fork: notes %q, offered %q", n, got)
	}
	step("b", func() { cloud.forkErr = nil })
	if n := notes(); n != "own summary text" {
		t.Fatalf("notes = %q", n)
	}
	step("c", func() {
		cloud.ownOut = ForkOutput{Text: editFind + "\nnot in the notes\n" + editSep + "\nx\n" + editReplace, Calls: 1}
	})
	if n, got := notes(), cloud.offeredOwn(); n != "own summary text" || len(got) != 1 {
		t.Fatalf("an update that does not apply: notes %q, offered %q", n, got)
	}
	step("d", func() {
		cloud.ownOut = ForkOutput{Text: editFind + "\n" + editSep + "\n" + strings.Repeat("hotel ", 100) + "\n" + editReplace, Calls: 1}
	})
	if got := cloud.offeredOwn(); len(got) != 2 || !strings.Contains(got[1], "[…]") || estimateText(got[1]) > 50 {
		t.Fatalf("notes over the cap: %q", got)
	}
	motels := strings.TrimSpace(strings.Repeat("motel ", 25))
	cut := notes()
	step("e", func() {
		cloud.ownOut = ForkOutput{Text: editFind + "\n" + cut + "\n" + editSep + "\nHotel list:\n" + motels + "\n" + editReplace, Calls: 1}
		cloud.offerErr = fmt.Errorf("%w: the master was rebuilt", ErrStale)
	})
	step("f", func() {
		cloud.ownOut = ForkOutput{Text: editFind + "\nHotel list:\n" + editSep + "\nInn list:\n" + editReplace + "\n" +
			editFind + "\n" + editSep + "\nnext: book\n" + editReplace, Calls: 1}
		cloud.offerErr = errors.New("disk full")
	})
	want := "Inn list:\n" + motels + "\nnext: book"
	if n, got := notes(), cloud.offeredOwn(); n != want || len(got) != 2 {
		t.Fatalf("after refused offers: notes %q, offered %q", n, got)
	}
	// A remark in place of the edits is dropped; NO CHANGES keeps the notes.
	step("g", func() {
		cloud.ownOut = ForkOutput{Text: "The notes are current.", Calls: 1}
		cloud.offerErr = nil
	})
	step("h", func() { cloud.ownOut = ForkOutput{Text: "NO CHANGES", Calls: 1} })
	if n, got := notes(), cloud.offeredOwn(); n != want || len(got) != 3 || got[2] != want {
		t.Fatalf("after a remark and NO CHANGES: notes %q, offered %q", n, got)
	}
}

// TestCoordinatorOwnNotesWaitForThePeerSummary: notes are offered only once a
// peer summary covers their snapshot, because installing them drops the
// history a peer summary still needs. Far from the participant's
// compaction line the notes are kept but not offered; near it a stale peer
// summary is written from the same snapshot first.
func TestCoordinatorOwnNotesWaitForThePeerSummary(t *testing.T) {
	env, cloud := ownEnv(t)
	cloud.setState(func(s *ParticipantState) { s.CompactAt = 100000 })
	var summaries int
	cloud.beforeOffer = func() { summaries = len(findEntries(env.log(), KindSummary, "")) }
	cloud.push(grow(700, "far from the line"), func(r *fakeRun) *ActivationEnd {
		r.f.setState(func(s *ParticipantState) { s.InputTokens, s.CompactAt = 1000, 1200 })
		return r.reply("near the line")
	})
	env.say("a")
	env.idle()
	if n := len(findEntries(env.log(), KindSummary, "")); n != 0 || len(cloud.offeredOwn()) != 0 {
		t.Fatalf("far from the line: %d peer summaries, offered %q", n, cloud.offeredOwn())
	}
	env.c.mu.Lock()
	kept := env.c.parts["cloud"].ownNotes
	env.c.mu.Unlock()
	if kept != "own summary text" {
		t.Fatalf("kept notes = %q", kept)
	}
	cloud.fmu.Lock()
	cloud.ownOut = ForkOutput{Text: noteNoChange, Calls: 1}
	cloud.fmu.Unlock()
	env.say("b")
	env.idle()
	if got := cloud.offeredOwn(); len(got) != 1 || summaries != 1 {
		t.Fatalf("near the line: offered %q after %d peer summaries", got, summaries)
	}
}

func TestCoordinatorForkWithoutSnapshot(t *testing.T) {
	env, _, cloud := forkEnv(t, nil, nil)
	cloud.snapErrs = []error{ErrNoSnapshot, errors.New("broken")}
	cloud.push(grow(500, "one"), grow(600, "two"), grow(700, "three"))
	for _, q := range []string{"a", "b", "c"} {
		env.say(q)
		env.idle()
	}
	if n := len(cloud.forkSpecs()); n != 1 {
		t.Fatalf("%d forks; want one after two snapshot failures", n)
	}
}

func TestCoordinatorForkFailureAndLossySummary(t *testing.T) {
	env, _, cloud := forkEnv(t, nil, nil)
	cloud.forkErr = errors.New("rate limited")
	cloud.peerOut = ForkOutput{Calls: 2}
	cloud.push(grow(500, "one"), grow(1000, "two"))
	env.say("a")
	env.idle()
	if n := len(findEntries(env.log(), KindSummary, "")); n != 0 {
		t.Fatalf("%d summaries after a failed fork", n)
	}
	if tot := env.c.Ledger().TotalsByKind()[UsageForkPeer]; tot.Calls != 2 || tot.UnknownCalls != 2 {
		t.Fatalf("failed fork ledger = %+v", tot)
	}

	cloud.fmu.Lock()
	cloud.forkErr = nil
	cloud.peerOut = ForkOutput{Text: strings.Repeat("y", 4000), Rejected: true, Calls: 2, Usage: &model.ChatUsage{InputTokens: 7}}
	cloud.fmu.Unlock()
	env.say("b")
	env.idle()
	sums := findEntries(env.log(), KindSummary, "")
	if len(sums) != 1 || !sums[0].Summary.Lossy {
		t.Fatalf("summaries = %+v", sums)
	}
	text := sums[0].Text()
	if n := utf8.RuneCountInString(text); n > 800 || !strings.Contains(text, "[…]") {
		t.Fatalf("lossy summary has %d runes: %.40q", n, text)
	}
}

func TestCoordinatorPeerForkOnHandoff(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		env, edge, cloud := forkEnv(t, nil, func(cfg *Config) {
			cfg.Summaries.PeerEvery = 1 << 30
			cfg.Summaries.Disabled = disabled
		})
		var oc uint64
		cloud.push(func(r *fakeRun) *ActivationEnd {
			r.say("over to you")
			var err error
			if oc, err = r.transfer("edge"); err != nil {
				t.Errorf("transfer: %v", err)
			}
			return &ActivationEnd{Reason: types.ReplyHandedOff}
		})
		edge.push(replyWith("hello"))
		env.say("go")
		env.idle()
		sums := findEntries(env.log(), KindSummary, "")
		if disabled {
			if len(sums) != 0 {
				t.Fatalf("summaries while disabled: %+v", sums)
			}
			continue
		}
		if len(sums) != 1 || sums[0].Coverage.Through != oc {
			t.Fatalf("handoff summary = %+v, transfer #%d", sums, oc)
		}
	}
}

func TestMiddleCut(t *testing.T) {
	if got := middleCut("short", 10); got != "short" {
		t.Fatalf("short text cut: %q", got)
	}
	got := middleCut(strings.Repeat("a", 50)+strings.Repeat("z", 50), 10)
	if utf8.RuneCountInString(got) != 40 || !strings.HasPrefix(got, "aaaa") || !strings.HasSuffix(got, "zzzz") {
		t.Fatalf("cut = %q", got)
	}
	if got := middleCut("abcdefgh", 1); got != "abcd" {
		t.Fatalf("tiny budget cut = %q", got)
	}
}

// TestCoordinatorPeerForkByEdgeWindow: without PeerEvery, the summary for the
// edge is refreshed when what a takeover would bring it (the newest summary
// and the entries after it) plus what the last reply added reaches 0.8 of
// the edge's window, and not before; after the refresh the load is small
// again.
func TestCoordinatorPeerForkByEdgeWindow(t *testing.T) {
	edge := newFakePart("edge", ZoneEdge)
	edge.desc.Window = 1000 // the limit is 800 tokens
	cloud := newFakeForker("cloud")
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Router = cloudRouter() }, edge, cloud)
	long := strings.Repeat("w", 1200) // about 300 tokens a reply
	cloud.push(replyWith(long), replyWith(long+"."), replyWith("short"))

	env.say("first")
	env.idle()
	if n := len(findEntries(env.log(), KindSummary, "")); n != 0 {
		t.Fatalf("a summary after the first reply (load about 320 + 320 < 800): %d", n)
	}
	env.say("second")
	env.idle()
	sums := findEntries(env.log(), KindSummary, "")
	if len(sums) != 1 {
		t.Fatalf("want one summary once load and rise pass 800: %d", len(sums))
	}
	env.say("third")
	env.idle()
	if n := len(findEntries(env.log(), KindSummary, "")); n != 1 {
		t.Fatalf("the refresh left a small load, yet another summary was written: %d", n)
	}
}

// TestPeerBudget: the peer summary's size follows the target: its
// SummaryBudget, else 5/6 of the summary budget it reports, else 5/16 of
// its window, else 1,024.
func TestPeerBudget(t *testing.T) {
	cases := []struct {
		name string
		d    Descriptor
		st   ParticipantState
		want int
	}{
		{"descriptor", Descriptor{SummaryBudget: 700, Window: 8192}, ParticipantState{SummaryTokens: 3000}, 700},
		{"reported budget", Descriptor{Window: 8192}, ParticipantState{SummaryTokens: 3000}, 2500},
		{"reported window", Descriptor{Window: 4096}, ParticipantState{Window: 8192}, 2560},
		{"descriptor window", Descriptor{Window: 8192}, ParticipantState{}, 2560},
		{"floor", Descriptor{Window: 400}, ParticipantState{}, 256},
		{"unknown", Descriptor{}, ParticipantState{}, 1024},
	}
	for _, tc := range cases {
		if got := peerBudget(&tc.d, tc.st); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}
