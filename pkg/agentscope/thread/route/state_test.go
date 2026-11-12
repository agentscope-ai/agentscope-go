package route

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
)

type decodedState struct {
	Query        *string            `json:"query"`
	Summary      *string            `json:"summary"`
	Recent       []stateEntry       `json:"recent"`
	Participants []stateParticipant `json:"participants"`
}

func decodeState(t *testing.T, raw json.RawMessage) decodedState {
	t.Helper()
	var s decodedState
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("state %s is not valid JSON: %v", raw, err)
	}
	return s
}

func mustBuild(t *testing.T, cfg Config, in thread.RouteRequest) (json.RawMessage, StateStats) { //nolint:gocritic // test helper
	t.Helper()
	raw, stats, err := BuildState(cfg, in)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}
	return raw, stats
}

func recentSeqs(s decodedState) []uint64 {
	out := make([]uint64, 0, len(s.Recent))
	for _, e := range s.Recent {
		out = append(out, e.Seq)
	}
	return out
}

func TestBuildStateShape(t *testing.T) {
	cloud := cloudCand(true, "web")
	cloud.Lag = 3
	in := thread.RouteRequest{
		Query:        "Book a table <tonight> & pay",
		Owner:        "edge",
		PeerSummary:  "User planned a trip.",
		Participants: []thread.RouteCandidate{cloud, edgeCand(true, "device")},
		Recent: []thread.Entry{
			{Seq: 10, Kind: thread.KindUserInput, Content: textBlocks("Find flights")},
			{Seq: 11, Kind: thread.KindReply, Origin: thread.Origin{Participant: "cloud", Path: "master"},
				Content: textBlocks("Found two."), Digest: &thread.ToolDigest{Calls: []thread.ToolCallDigest{{Name: "WebSearch", Args: `{"query":"x"}`}, {Name: "WebSearch", Args: `{"query":"y"}`}}}},
			{Seq: 12, Kind: thread.KindSummary, Content: textBlocks("a summary entry")},
			{Seq: 13, Kind: thread.KindMarker, Marker: &thread.Marker{Kind: thread.MarkerInterrupted}},
			{Seq: 14, Kind: thread.KindOwnerChange, Owner: &thread.OwnerChange{From: "cloud", To: "edge", Reason: thread.ReasonRouter}},
			{Seq: 15, Kind: thread.KindArtifact, Origin: thread.Origin{Participant: "edge"},
				Refs: []thread.ArtifactRef{{URI: "file://plan.md", Summary: "trip plan"}}},
			{Seq: 16, Kind: thread.KindAssign, Origin: thread.Origin{Participant: "cloud", Path: "master"},
				Content: textBlocks("Check the calendar"), Task: &thread.Task{ID: "t1"}},
			{Seq: 17, Kind: thread.KindOwnerChange, Owner: &thread.OwnerChange{To: "cloud", Reason: thread.ReasonTransfer,
				Brief: &thread.Brief{Goal: "Pay", Remaining: []string{"pay the deposit"}}}},
		},
	}
	raw, stats := mustBuild(t, Config{IncludeHistory: true}, in)
	want := `{"query":"Book a table <tonight> & pay","summary":"User planned a trip.","recent":[` +
		`{"seq":10,"kind":"user_input","from":"user","text":"Find flights"},` +
		`{"seq":11,"kind":"reply","from":"cloud/master","text":"Found two.","tools":"WebSearch ×2"},` +
		`{"seq":14,"kind":"owner_change","from":"coordinator","text":"foreground: cloud → edge, reason router"},` +
		`{"seq":15,"kind":"artifact","from":"edge","text":"file://plan.md: trip plan"},` +
		`{"seq":16,"kind":"assign","from":"cloud/master","text":"Check the calendar"},` +
		`{"seq":17,"kind":"owner_change","from":"coordinator","text":"foreground: none → cloud, reason transfer\nbrief: Goal: Pay\nRemaining:\n- pay the deposit"}],` +
		`"participants":[{"name":"cloud","zone":"cloud","window":200000,"available":true,"lag":3,"tools":["web"]},` +
		`{"name":"edge","zone":"edge","window":8192,"available":true,"lag":0,"tools":["device"],"owner":true}]}`
	if string(raw) != want {
		t.Fatalf("state\n got %s\nwant %s", raw, want)
	}
	if stats.RecentKept != 6 || stats.RecentFiltered != 2 || stats.RecentDropped != 0 || stats.QueryCut || stats.SummaryCut {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.TotalTokens != defaultCount(want) || stats.QueryTokens != defaultCount(in.Query) || stats.SummaryTokens != defaultCount(in.PeerSummary) {
		t.Fatalf("token counts = %+v", stats)
	}
	if stats.ParticipantTokens == 0 || stats.RecentTokens == 0 {
		t.Fatalf("part counts missing: %+v", stats)
	}
}

func TestBuildStateDeterministic(t *testing.T) {
	in := request()
	in.PeerSummary = "summary"
	for i := 1; i <= 20; i++ {
		in.Recent = append(in.Recent, thread.Entry{Seq: uint64(i), Kind: thread.KindUserInput, Content: textBlocks(fmt.Sprintf("message %d", i))})
	}
	first, _ := mustBuild(t, Config{IncludeHistory: true}, in)
	again, _ := mustBuild(t, Config{IncludeHistory: true}, in)
	if string(first) != string(again) {
		t.Fatalf("same input gave different bytes:\n%s\n%s", first, again)
	}
	in.Participants = []thread.RouteCandidate{in.Participants[1], in.Participants[0]}
	reordered, _ := mustBuild(t, Config{IncludeHistory: true}, in)
	if string(first) != string(reordered) {
		t.Fatalf("participant order changed the bytes:\n%s\n%s", first, reordered)
	}
}

func TestBuildStateRecentBudget(t *testing.T) {
	in := request()
	for i := 1; i <= 30; i++ {
		in.Recent = append(in.Recent, thread.Entry{Seq: uint64(i), Kind: thread.KindUserInput, Content: textBlocks(fmt.Sprintf("message number %02d", i))})
	}
	// Each entry encodes to 71 runes, 18 tokens: three fit in 60.
	raw, stats := mustBuild(t, Config{IncludeHistory: true, Budget: StateBudget{Recent: 60}}, in)
	s := decodeState(t, raw)
	if got := recentSeqs(s); !slices.Equal(got, []uint64{28, 29, 30}) {
		t.Fatalf("recent seqs = %v, want the newest three in log order", got)
	}
	if stats.RecentKept != 3 || stats.RecentDropped != 27 || stats.RecentTokens != 54 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestBuildStateEntryText(t *testing.T) {
	in := request()
	in.Recent = []thread.Entry{{Seq: 1, Kind: thread.KindReport, Origin: thread.Origin{Participant: "edge"}, Content: textBlocks(strings.Repeat("é", 1000))}}
	raw, _ := mustBuild(t, Config{IncludeHistory: true}, in)
	s := decodeState(t, raw)
	text := s.Recent[0].Text
	if n := utf8.RuneCountInString(text); n != entryTextRunes || !strings.HasSuffix(text, "…") {
		t.Fatalf("entry text has %d runes, ends %q", n, text[len(text)-6:])
	}
}

func TestBuildStateQueryCut(t *testing.T) {
	in := request()
	in.Query = "HEAD " + strings.Repeat("x", 10000) + " TAIL"
	raw, stats := mustBuild(t, Config{IncludeHistory: true, Budget: StateBudget{Query: 100}}, in)
	q := *decodeState(t, raw).Query
	if !strings.HasPrefix(q, "HEAD ") || !strings.HasSuffix(q, " TAIL") || !strings.Contains(q, cutMarker) {
		t.Fatalf("query was not cut in the middle: %q", q)
	}
	if !stats.QueryCut || stats.QueryTokens > 100 || stats.QueryTokens < 95 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestBuildStateSummary(t *testing.T) {
	in := request()
	in.PeerSummary = "the summary"
	in.SummaryStale = true
	raw, stats := mustBuild(t, Config{IncludeHistory: true}, in)
	if decodeState(t, raw).Summary != nil || !stats.SummaryStale {
		t.Fatalf("a stale summary should be left out: %s, %+v", raw, stats)
	}

	in.SummaryStale = false
	in.PeerSummary = strings.Repeat("s", 4000)
	raw, stats = mustBuild(t, Config{IncludeHistory: true}, in)
	if got := *decodeState(t, raw).Summary; got != in.PeerSummary || stats.SummaryCut {
		t.Fatal("with Budget.Summary zero the summary is kept as written")
	}
	raw, stats = mustBuild(t, Config{IncludeHistory: true, Budget: StateBudget{Summary: 50}}, in)
	if got := *decodeState(t, raw).Summary; !strings.Contains(got, cutMarker) || !stats.SummaryCut || stats.SummaryTokens > 50 {
		t.Fatalf("summary %q, stats %+v", got, stats)
	}

	in.PeerSummary = ""
	raw, _ = mustBuild(t, Config{IncludeHistory: true}, in)
	if strings.Contains(string(raw), `"summary"`) {
		t.Fatalf("an empty summary should be left out: %s", raw)
	}
}

func TestBuildStateTotalBudget(t *testing.T) {
	in := request()
	in.PeerSummary = "START " + strings.Repeat("summary words ", 300) + " END"
	for i := 1; i <= 5; i++ {
		in.Recent = append(in.Recent, thread.Entry{Seq: uint64(i), Kind: thread.KindUserInput, Content: textBlocks("question")})
	}
	budget := StateBudget{Query: 50, Participants: 100, Total: 400}
	raw, stats := mustBuild(t, Config{IncludeHistory: true, Budget: budget}, in)
	s := decodeState(t, raw)
	if len(s.Recent) != 0 || stats.RecentDroppedForTotal != 5 {
		t.Fatalf("recent = %v, stats %+v", recentSeqs(s), stats)
	}
	if !stats.SummaryCutForTotal || !strings.HasPrefix(*s.Summary, "START") || !strings.HasSuffix(*s.Summary, "END") || !strings.Contains(*s.Summary, cutMarker) {
		t.Fatalf("summary %q, stats %+v", *s.Summary, stats)
	}
	if stats.TotalTokens > 400 || stats.TotalTokens != defaultCount(string(raw)) || stats.TotalTokens < 395 {
		t.Fatalf("total %d tokens", stats.TotalTokens)
	}

	// Recent entries are dropped oldest first, and the summary is cut only
	// when that is not enough. Each entry below is cut to 400 runes, about
	// 114 tokens, so six of them (684) fit Budget.Recent but not the total.
	in.PeerSummary = "short summary"
	big := strings.Repeat("w", 1200)
	in.Recent = nil
	for i := 1; i <= 6; i++ {
		in.Recent = append(in.Recent, thread.Entry{Seq: uint64(i), Kind: thread.KindUserInput, Content: textBlocks(big)})
	}
	raw, stats = mustBuild(t, Config{IncludeHistory: true, Budget: StateBudget{Query: 100, Participants: 100, Recent: 2000, Total: 700}}, in)
	s = decodeState(t, raw)
	if got := recentSeqs(s); !slices.Equal(got, []uint64{2, 3, 4, 5, 6}) || stats.RecentDroppedForTotal != 1 || stats.SummaryCutForTotal {
		t.Fatalf("recent %v, stats %+v", got, stats)
	}
	if *s.Summary != "short summary" || stats.TotalTokens > 700 {
		t.Fatalf("summary %q, stats %+v", *s.Summary, stats)
	}
}

func TestBuildStateSummaryDropped(t *testing.T) {
	// One token per rune makes the arithmetic exact.
	count := utf8.RuneCountInString
	in := request()
	in.Query = "hi"
	base, baseStats := mustBuild(t, Config{IncludeHistory: true, Count: count, Budget: StateBudget{Query: 10, Participants: 220, Total: 2000}}, in)
	if baseStats.TotalTokens != len([]rune(string(base))) {
		t.Fatalf("total %d for %d runes", baseStats.TotalTokens, len([]rune(string(base))))
	}
	// A summary needs at least `"summary":"…[cut]…",`, 20 runes: give it 5.
	in.PeerSummary = strings.Repeat("s", 500)
	raw, stats := mustBuild(t, Config{IncludeHistory: true, Count: count, Budget: StateBudget{Query: 10, Participants: 220, Total: baseStats.TotalTokens + 5}}, in)
	if !stats.SummaryDropped || decodeState(t, raw).Summary != nil || string(raw) != string(base) {
		t.Fatalf("state %s, stats %+v", raw, stats)
	}
	if !strings.Contains(stats.trimNote(), "left out the summary") {
		t.Fatalf("note = %q", stats.trimNote())
	}
}

func TestBuildStateParticipantBudget(t *testing.T) {
	in := request()
	var groups []string
	for i := range 60 {
		groups = append(groups, fmt.Sprintf("group-%02d", i))
	}
	in.Participants = []thread.RouteCandidate{edgeCand(true, groups...), cloudCand(true, groups...)}
	raw, stats := mustBuild(t, Config{IncludeHistory: true}, in)
	s := decodeState(t, raw)
	if !stats.ToolsDropped || len(s.Participants) != 2 || s.Participants[0].Tools != nil || stats.ParticipantTokens > 300 {
		t.Fatalf("participants %+v, stats %+v", s.Participants, stats)
	}

	// A participant list over its budget even without its tool groups fails
	// at build time, with history and without.
	for _, history := range []bool{true, false} {
		_, _, err := BuildState(Config{Budget: StateBudget{Query: 1, Participants: 5, Total: 100}, IncludeHistory: history}, in)
		if err == nil || errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "the participant list needs") {
			t.Fatalf("history %v, a participant list over budget without tools: %v", history, err)
		}
	}
}

func TestBuildStateErrors(t *testing.T) {
	if _, _, err := BuildState(Config{Budget: StateBudget{Total: -1}}, request()); !errors.Is(err, ErrConfig) {
		t.Fatalf("negative budget: %v", err)
	}
	huge := func(s string) int {
		if strings.Contains(s, `"participants":`) {
			return 1_000_000
		}
		return defaultCount(s)
	}
	in := request()
	in.PeerSummary = "a summary"
	in.Recent = []thread.Entry{{Seq: 1, Kind: thread.KindUserInput, Content: textBlocks("hi")}}
	for _, history := range []bool{true, false} {
		_, _, err := BuildState(Config{Count: huge, IncludeHistory: history}, in)
		if err == nil || !strings.Contains(err.Error(), "over the total budget") {
			t.Fatalf("history %v, an unfittable state: %v", history, err)
		}
	}
}

func TestCutHelpers(t *testing.T) {
	if got, ok := cutMiddle("abcdef", func(string) bool { return false }); ok || got != cutMarker {
		t.Fatalf("cutMiddle with nothing fitting = %q, %v", got, ok)
	}
	got, ok := cutMiddle("abcdefgh", func(s string) bool { return utf8.RuneCountInString(s) <= utf8.RuneCountInString(cutMarker)+3 })
	if !ok || got != "ab"+cutMarker+"h" {
		t.Fatalf("cutMiddle = %q, %v", got, ok)
	}
	if cutEnd("short", 10) != "short" || cutEnd("abcdef", 4) != "abc…" {
		t.Fatal("cutEnd")
	}
}

// TestBuildStateReportsEncodeFailures: whichever encoding fails (a recent
// entry, the participant list, the total or the final document), the build
// returns the error and no state.
func TestBuildStateReportsEncodeFailures(t *testing.T) {
	defer func(orig func(any) ([]byte, error)) { encode = orig }(encode)
	in := request()
	in.PeerSummary = "User planned a trip."
	in.Recent = []thread.Entry{{Seq: 10, Kind: thread.KindUserInput, Content: textBlocks("Find flights")}}
	boom := errors.New("boom")
	// With history the build encodes a recent entry, the participant list,
	// the total and the final document; without it, the participant list
	// and the document.
	for _, tc := range []struct {
		history bool
		least   int
	}{{true, 5}, {false, 2}} {
		for fail := 1; ; fail++ {
			calls := 0
			encode = func(v any) ([]byte, error) {
				if calls++; calls == fail {
					return nil, boom
				}
				return encodeJSON(v)
			}
			raw, _, err := BuildState(Config{IncludeHistory: tc.history}, in)
			if calls < fail {
				if err != nil || raw == nil {
					t.Fatalf("history %v, with no failure: %v", tc.history, err)
				}
				if fail < tc.least {
					t.Fatalf("history %v: the build made only %d encodings", tc.history, calls)
				}
				break
			}
			if !errors.Is(err, boom) || raw != nil {
				t.Fatalf("history %v, encoding %d failed: raw=%s err=%v", tc.history, fail, raw, err)
			}
		}
	}
}

// TestBuildStateWithoutHistory: by default the state holds the query and the
// participants only, with no summary, recent entries, lag or ownership, as a
// classifier that routes from the query alone sees a request.
func TestBuildStateWithoutHistory(t *testing.T) {
	cloud := cloudCand(true, "web")
	cloud.Lag = 3
	in := thread.RouteRequest{
		Query:        "Book a table <tonight> & pay",
		Owner:        "edge",
		PeerSummary:  "User planned a trip.",
		Participants: []thread.RouteCandidate{cloud, edgeCand(true, "device")},
		Recent:       []thread.Entry{{Seq: 10, Kind: thread.KindUserInput, Content: textBlocks("Find flights")}},
	}
	raw, stats := mustBuild(t, Config{}, in)
	want := `{"query":"Book a table <tonight> & pay","participants":[` +
		`{"name":"cloud","zone":"cloud","window":200000,"available":true,"tools":["web"]},` +
		`{"name":"edge","zone":"edge","window":8192,"available":true,"tools":["device"]}]}`
	if string(raw) != want {
		t.Fatalf("state\n got %s\nwant %s", raw, want)
	}
	if stats.TotalTokens != defaultCount(want) || stats.QueryTokens != defaultCount(in.Query) || stats.ParticipantTokens == 0 ||
		stats.SummaryTokens != 0 || stats.RecentTokens != 0 || stats.RecentKept != 0 || stats.SummaryStale {
		t.Fatalf("stats = %+v", stats)
	}
	// A long query is still cut to its budget.
	in.Query = strings.Repeat("q", 4000)
	_, stats = mustBuild(t, Config{Budget: StateBudget{Query: 100}}, in)
	if !stats.QueryCut || stats.QueryTokens > 100 {
		t.Fatalf("stats = %+v", stats)
	}
	// A state over its total budget is an error, as with history. One token
	// per rune makes the arithmetic exact: the keys alone overflow a total
	// that just fits the query and participant budgets.
	count := utf8.RuneCountInString
	_, base := mustBuild(t, Config{Count: count, Budget: StateBudget{Query: 10, Participants: 300, Total: 2000}}, in)
	tight := StateBudget{Query: 10, Participants: base.ParticipantTokens, Total: 10 + base.ParticipantTokens + 1}
	if base.TotalTokens <= tight.Total {
		t.Fatalf("total %d leaves no overflow to test", base.TotalTokens)
	}
	if _, _, err := BuildState(Config{Count: count, Budget: tight}, in); err == nil || errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "over the total budget") {
		t.Fatalf("an over-budget state without history: %v", err)
	}
}
