package thread

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func at1(seq uint64, e Entry) Entry { //nolint:gocritic // test helper
	e.Seq = seq
	return e
}

func texts(msgs []*message.Msg) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, msgText(m))
	}
	return out
}

func TestProjectRendersEveryKind(t *testing.T) {
	cloudMaster := Origin{Participant: "cloud", Path: "master"}
	cases := []struct {
		name  string
		entry Entry
		want  string
	}{
		{"user input", at1(12, userInput("hello")), "[#12 user] hello"},
		{"reply with digest", Entry{Seq: 13, Kind: KindReply, Origin: cloudMaster, Content: textBlocks("done"),
			Digest: &ToolDigest{Calls: []ToolCallDigest{{Name: "Read", Args: `{"path":"a.go"}`}, {Name: "Grep"}}}},
			"[#13 reply from cloud/master] done\nTools:\nRead {\"path\":\"a.go\"}\nGrep"},
		{"reply without digest", Entry{Seq: 13, Kind: KindReply, Origin: cloudMaster, Content: textBlocks("done")},
			"[#13 reply from cloud/master] done"},
		{"reply with only a digest", Entry{Seq: 13, Kind: KindReply, Origin: cloudMaster,
			Digest: &ToolDigest{Calls: []ToolCallDigest{{Name: "Bash"}}}},
			"[#13 reply from cloud/master]\nTools:\nBash"},
		{"owner change with brief", Entry{Seq: 14, Kind: KindOwnerChange,
			Owner: &OwnerChange{From: "edge", To: "cloud", Reason: ReasonTransfer, Brief: &Brief{Goal: "fix", Remaining: []string{"test"}}}},
			"[#14 foreground: edge → cloud, reason transfer]\nBrief:\nGoal: fix\nRemaining:\n- test"},
		{"first owner change", Entry{Seq: 14, Kind: KindOwnerChange, Owner: &OwnerChange{To: "edge", Reason: ReasonRouter}},
			"[#14 foreground: none → edge, reason router]"},
		{"assign", Entry{Seq: 15, Kind: KindAssign, Origin: cloudMaster, To: []string{"edge"},
			Task: &Task{ID: "t-3"}, Content: textBlocks("scan the disk")},
			"[#15 task t-3 from cloud/master to edge] scan the disk"},
		{"report", at1(16, report("edge", "found 3")), "[#16 report from edge] found 3"},
		{"report to peers", Entry{Seq: 16, Kind: KindReport, Origin: Origin{Participant: "edge"},
			To: []string{"cloud", "cloud2"}, Content: textBlocks("found 3")},
			"[#16 report from edge to cloud, cloud2] found 3"},
		{"artifact", Entry{Seq: 17, Kind: KindArtifact, Origin: cloudMaster, Content: textBlocks("both ready"),
			Refs: []ArtifactRef{{URI: "s3://b/a.csv", Summary: "rows", Size: 2048}, {URI: "s3://b/b.csv"}}},
			"[#17 artifact from cloud/master] s3://b/a.csv — rows (2048 bytes)\ns3://b/b.csv\nboth ready"},
		{"artifact with text only", Entry{Seq: 17, Kind: KindArtifact, Origin: cloudMaster, Content: textBlocks("pending")},
			"[#17 artifact from cloud/master] pending"},
		{"marker", Entry{Seq: 18, Kind: KindMarker, Marker: &Marker{Kind: MarkerUnknownEffects, Detail: "rm may have run"}},
			"[#18 marker unknown_effects] rm may have run"},
		{"marker with text", Entry{Seq: 18, Kind: KindMarker, Marker: &Marker{Kind: MarkerRejected}, Content: textBlocks("late")},
			"[#18 marker rejected_output]\nlate"},
		{"unknown kind", Entry{Seq: 19, Kind: "note", Origin: cloudMaster, Content: textBlocks("x")},
			"[#19 note from cloud/master] x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Project([]Entry{tc.entry}, &ProjectionSpec{Participant: "observer"})
			if len(p.Messages) != 1 {
				t.Fatalf("%d messages, want 1", len(p.Messages))
			}
			m := p.Messages[0]
			if got := msgText(m); got != tc.want {
				t.Errorf("text\n got %q\nwant %q", got, tc.want)
			}
			if m.Name != "thread" || m.Role != message.RoleUser {
				t.Errorf("message from %q as %q, want a user message from thread", m.Name, m.Role)
			}
			if seq, ok := m.Metadata[SeqMetadataKey].(uint64); !ok || seq != tc.entry.Seq {
				t.Errorf("seq metadata %#v, want %d", m.Metadata[SeqMetadataKey], tc.entry.Seq)
			}
			if kind := m.Metadata[KindMetadataKey]; kind != string(tc.entry.Kind) {
				t.Errorf("kind metadata %#v, want %q", kind, tc.entry.Kind)
			}
			if p.Through != tc.entry.Seq || p.Remaining {
				t.Errorf("projection %+v", p)
			}
		})
	}
}

func TestProjectSkips(t *testing.T) {
	entries := []Entry{
		at1(1, userInput("old")),
		at1(2, userInput("old")),
		at1(3, userInput("covered")),
		at1(4, userInput("excluded from the summary")),
		at1(5, replyFrom("edge", "mine")),
		at1(6, Entry{Kind: KindSummary, Coverage: &Coverage{Through: 4}, Summary: &SummaryMeta{Target: "edge"}}),
		at1(7, userInput("new")),
		at1(8, report("cloud", "result")),
	}
	spec := ProjectionSpec{Participant: "edge", After: 2, Covered: &Coverage{Through: 4, Excluded: []uint64{4}}}
	cases := []struct {
		name   string
		mutate func(*ProjectionSpec)
		want   []string
	}{
		{"own and covered skipped", func(*ProjectionSpec) {}, []string{
			"[#4 user] excluded from the summary", "[#7 user] new", "[#8 report from cloud] result",
		}},
		{"include own", func(s *ProjectionSpec) { s.IncludeOwn = true }, []string{
			"[#4 user] excluded from the summary", "[#5 reply from edge/master] mine", "[#7 user] new", "[#8 report from cloud] result",
		}},
		{"no coverage", func(s *ProjectionSpec) { s.Covered = nil }, []string{
			"[#3 user] covered", "[#4 user] excluded from the summary", "[#7 user] new", "[#8 report from cloud] result",
		}},
		{"anonymous participant skips nothing", func(s *ProjectionSpec) { s.Participant = "" }, []string{
			"[#4 user] excluded from the summary", "[#5 reply from edge/master] mine", "[#7 user] new", "[#8 report from cloud] result",
		}},
		{"cursor at the end", func(s *ProjectionSpec) { s.After = 8 }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := spec
			tc.mutate(&s)
			p := Project(entries, &s)
			if got := texts(p.Messages); !reflect.DeepEqual(got, tc.want) && (len(got) > 0 || len(tc.want) > 0) {
				t.Fatalf("messages\n got %q\nwant %q", got, tc.want)
			}
			if p.Through != 8 || p.Remaining {
				t.Errorf("through %d remaining %v", p.Through, p.Remaining)
			}
		})
	}

	// Skipped entries at the end still move the cursor.
	p := Project(entries[:6], &ProjectionSpec{Participant: "edge", After: 3})
	if p.Through != 6 || len(p.Messages) != 1 {
		t.Errorf("trailing skips: through %d with %d messages, want 6 with 1", p.Through, len(p.Messages))
	}
}

// TestProjectShowsOnlyLiveProgress: a projection renders the newest progress
// entry of a run, and none of a run that ended unless it failed. A restored
// participant's own live progress comes back even when it is based on its
// cursor.
func TestProjectShowsOnlyLiveProgress(t *testing.T) {
	progress := func(seq uint64, who, run, plan string) Entry {
		return at1(seq, Entry{Kind: KindProgress, Origin: Origin{Participant: who}, BasedOn: 1, Run: run,
			Content: textBlocks(plan), DedupKey: fmt.Sprintf("progress:%s:%d", run, seq)})
	}
	end := func(seq uint64, e Entry, key string) Entry {
		e.DedupKey = key
		return at1(seq, e)
	}
	failed := markerEntry(MarkerFailed, 1, "cloud failed: down")
	entries := []Entry{
		at1(1, userInput("plan my day")),
		progress(2, "cloud", "a", "a: first"),
		progress(3, "cloud", "a", "a: second"),
		progress(4, "cloud", "b", "b: only"),
		end(5, replyFrom("cloud", "b done"), "reply:b"),
		progress(6, "cloud", "c", "c: only"),
		end(7, replyFrom("cloud", "c partial"), "reply:c"),
		end(8, failed, "failed:c"),
		progress(9, "edge", "d", "d: mine"),
	}
	p := Project(entries, &ProjectionSpec{Participant: "edge"})
	want := []string{
		"[#1 user] plan my day",
		"[#3 progress of cloud]\na: second",
		"[#5 reply from cloud/master] b done",
		"[#6 progress of cloud]\nc: only",
		"[#7 reply from cloud/master] c partial",
		"[#8 marker failed] cloud failed: down",
	}
	if got := texts(p.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("messages\n got %q\nwant %q", got, want)
	}
	if p.Through != 9 {
		t.Fatalf("through %d, want 9", p.Through)
	}
	// After a restart the participant's own live progress comes back, though
	// it is based on the cursor; its other own entries there do not.
	p = Project(entries, &ProjectionSpec{Participant: "edge", After: 8, OwnBasedAfter: 1})
	if got := texts(p.Messages); !reflect.DeepEqual(got, []string{"[#9 progress of edge]\nd: mine"}) {
		t.Fatalf("restored own progress: %q", got)
	}
	// A run that published one plan and ended leaves no progress; a nil spec
	// is the zero one.
	short := []Entry{at1(1, userInput("hi")), progress(2, "cloud", "e", "e: only"), end(3, replyFrom("cloud", "e done"), "reply:e")}
	if got := texts(Project(short, nil).Messages); !reflect.DeepEqual(got, []string{"[#1 user] hi", "[#3 reply from cloud/master] e done"}) {
		t.Fatalf("ended run: %q", got)
	}
	// An interrupt that leaves no reply still ends the run.
	stopped := []Entry{at1(1, userInput("hi")), progress(2, "cloud", "f", "f: only"),
		end(3, markerEntry(MarkerInterrupted, 1, "cloud was interrupted while answering #1"), interruptedPrefix+"f")}
	if got := texts(Project(stopped, nil).Messages); len(got) != 2 || strings.Contains(strings.Join(got, "\n"), "f: only") {
		t.Fatalf("interrupted run: %q", got)
	}
}

func TestProjectBudget(t *testing.T) {
	ten := func(*message.Msg) int { return 10 }
	entries := []Entry{
		at1(1, userInput("a")),
		at1(2, userInput("b")),
		at1(3, Entry{Kind: KindSummary, Coverage: &Coverage{}, Summary: &SummaryMeta{Target: "x"}}),
		at1(4, userInput("c")),
	}
	cases := []struct {
		max       int
		messages  int
		through   uint64
		remaining bool
	}{
		{max: 0, messages: 3, through: 4},
		{max: 25, messages: 2, through: 3, remaining: true},
		{max: 20, messages: 2, through: 3, remaining: true},
		{max: 30, messages: 3, through: 4},
		{max: 10, messages: 1, through: 1, remaining: true},
	}
	for _, tc := range cases {
		p := Project(entries, &ProjectionSpec{MaxTokens: tc.max, Count: ten})
		if len(p.Messages) != tc.messages || p.Through != tc.through || p.Remaining != tc.remaining {
			t.Errorf("max %d: %d messages through %d remaining %v; want %d through %d remaining %v",
				tc.max, len(p.Messages), p.Through, p.Remaining, tc.messages, tc.through, tc.remaining)
		}
	}

}

func TestProjectCutsOversizeFirstMessage(t *testing.T) {
	long := strings.Repeat("x", 400) + "TAIL"
	entries := []Entry{at1(9, userInput(long)), at1(10, userInput("short"))}
	p := Project(entries, &ProjectionSpec{MaxTokens: 30})
	if len(p.Messages) != 1 || p.Through != 9 || !p.Remaining {
		t.Fatalf("%d messages through %d remaining %v; want the cut entry alone", len(p.Messages), p.Through, p.Remaining)
	}
	if !reflect.DeepEqual(p.Cut, []uint64{9}) {
		t.Fatalf("Cut = %v, want [9]", p.Cut)
	}
	if whole := Project(entries[1:], &ProjectionSpec{MaxTokens: 30}); len(whole.Cut) != 0 {
		t.Fatalf("an entry that fits is reported cut: %v", whole.Cut)
	}
	got := msgText(p.Messages[0])
	notice := "… [cut; read #9 in full with thread_read]"
	if !strings.HasPrefix(got, "[#9 user] xxx") || !strings.HasSuffix(got, notice) || strings.Contains(got, "TAIL") {
		t.Fatalf("cut text %q", got)
	}
	// The default counter allows (runes+3)/4 <= 30, so the cut keeps 120 runes.
	if n := utf8.RuneCountInString(got); n != 120 {
		t.Errorf("cut text has %d runes, want 120", n)
	}
	if n := estimateTokens(p.Messages[0]); n > 30 {
		t.Errorf("cut message counts %d tokens, budget 30", n)
	}

	tiny := Project(entries, &ProjectionSpec{MaxTokens: 1})
	if got := texts(tiny.Messages); len(got) != 1 || got[0] != notice || tiny.Through != 9 {
		t.Errorf("budget below the notice: %q through %d", got, tiny.Through)
	}
}

func TestEstimateTokens(t *testing.T) {
	for text, want := range map[string]int{"": 0, "a": 1, "abcd": 1, "abcde": 2, "日本語です": 2} {
		if got := estimateTokens(message.UserMsg("u", text)); got != want {
			t.Errorf("estimateTokens(%q) = %d, want %d", text, got, want)
		}
	}
	if got := estimateTokens(message.UserMsg("u", []message.ContentBlock{message.DataBlock{}})); got != 0 {
		t.Errorf("message without text counts %d", got)
	}
}
