package thread

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestEntryJSONRoundTrip(t *testing.T) {
	in := Entry{
		Seq:     7,
		Epoch:   2,
		Kind:    KindReply,
		Origin:  Origin{Participant: "cloud", Path: "master"},
		To:      []string{"edge"},
		BasedOn: 5,
		Content: []message.ContentBlock{
			message.TextBlock{Type: "text", ID: "t1", Text: "here is the chart"},
			message.DataBlock{Type: "data", ID: "d1", Name: "chart.png", Source: message.Base64Source{Type: "base64", Data: "aGk=", MediaType: "image/png"}},
			message.DataBlock{Type: "data", ID: "d2", Source: message.URLSource{Type: "url", URL: "https://example.com/a.mp3", MediaType: "audio/mpeg"}},
		},
		Digest:   &ToolDigest{Calls: []ToolCallDigest{{Name: "Read", Args: `{"path":"a.go"}`}}},
		Refs:     []ArtifactRef{{URI: "file:///chart.png", Summary: "chart", Size: 2048}},
		Coverage: &Coverage{Through: 4, Excluded: []uint64{3}},
		Summary:  &SummaryMeta{Target: "edge", Budget: 900, Lossy: true},
		Owner:    &OwnerChange{From: "edge", To: "cloud", Reason: ReasonTransfer, Brief: &Brief{Goal: "g", Remaining: []string{"r"}}},
		Task:     &Task{ID: "t-3", Notify: true},
		Marker:   &Marker{Kind: MarkerUnknownEffects, Detail: "d"},
		Notify:   true,
		DedupKey: "k",
		Time:     testTime,
	}
	for name, v := range map[string]any{"value": in, "pointer": &in, "slice": []Entry{in}} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(data), `"content":[`) {
				t.Fatalf("content missing from %s", data)
			}
			var out Entry
			if name == "slice" {
				var batch []Entry
				if err := json.Unmarshal(data, &batch); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				out = batch[0]
			} else if err := json.Unmarshal(data, &out); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(out, in) {
				t.Fatalf("round trip changed the entry:\n got %#v\nwant %#v", out, in)
			}
		})
	}
}

func TestEntryJSONWithoutContent(t *testing.T) {
	data, err := json.Marshal(Entry{Kind: KindMarker, Marker: &Marker{Kind: MarkerNoOwner}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "content") {
		t.Fatalf("empty content encoded: %s", data)
	}
	var out Entry
	if err := json.Unmarshal([]byte(`{"kind":"marker","content":null}`), &out); err != nil || out.Content != nil {
		t.Fatalf("null content: %v, %#v", err, out.Content)
	}
	if err := json.Unmarshal([]byte(`{"kind":"marker","content":"text"}`), &out); err == nil {
		t.Fatal("content that is not an array decoded")
	}
	if err := json.Unmarshal([]byte(`{"seq":"x"}`), &out); err == nil {
		t.Fatal("a bad seq decoded")
	}
}

func TestEntryHelpers(t *testing.T) {
	e := Entry{To: []string{"edge"}, Content: []message.ContentBlock{
		message.TextBlock{Text: "a"}, message.DataBlock{}, message.TextBlock{Text: ""}, message.TextBlock{Text: "b"},
	}}
	if got := e.Text(); got != "a\nb" {
		t.Errorf("Text = %q", got)
	}
	if !e.Addressed("edge") || e.Addressed("cloud") {
		t.Error("Addressed")
	}
	for o, want := range map[Origin]string{{}: "coordinator", {Participant: "edge"}: "edge", {Participant: "cloud", Path: "master"}: "cloud/master"} {
		if got := o.String(); got != want {
			t.Errorf("Origin%+v.String() = %q, want %q", o, got, want)
		}
	}
	c := &Coverage{Through: 5, Excluded: []uint64{3}}
	for seq, want := range map[uint64]bool{1: true, 3: false, 5: true, 6: false} {
		if got := c.Covers(seq); got != want {
			t.Errorf("Covers(%d) = %v", seq, got)
		}
	}
	if (*Coverage)(nil).Covers(1) {
		t.Error("nil coverage covers")
	}
	if (*ToolDigest)(nil).Render() != "" || (&ToolDigest{}).Render() != "" {
		t.Error("empty digest renders")
	}
}

func TestBrief(t *testing.T) {
	b := &Brief{
		Goal:        " ship the fix ",
		Done:        []string{"found the bug"},
		Remaining:   []string{"run the tests "},
		Constraints: []string{"no new deps"},
		Refs:        []ArtifactRef{{URI: "file:///log.txt", Summary: "failing log"}, {URI: "file:///b"}},
	}
	want := "Goal: ship the fix\nDone:\n- found the bug\nRemaining:\n- run the tests\nConstraints:\n- no new deps\n" +
		"Artifacts:\n- file:///log.txt: failing log\n- file:///b"
	if got := b.Render(); got != want {
		t.Fatalf("Render\n got %q\nwant %q", got, want)
	}
	if (*Brief)(nil).Render() != "" {
		t.Error("nil brief renders")
	}
	if err := b.Validate(0); err != nil {
		t.Errorf("Validate: %v", err)
	}
	if err := b.Validate(len([]rune(want))); err != nil {
		t.Errorf("Validate at the exact size: %v", err)
	}
	cases := map[string]struct {
		b   *Brief
		max int
	}{
		"nil":          {nil, 0},
		"no goal":      {&Brief{Goal: " ", Remaining: []string{"x"}}, 0},
		"no remaining": {&Brief{Goal: "g"}, 0},
		"too long":     {b, len([]rune(want)) - 1},
	}
	for name, tc := range cases {
		if err := tc.b.Validate(tc.max); err == nil {
			t.Errorf("%s: Validate accepted the brief", name)
		}
	}
}

func TestValidateContent(t *testing.T) {
	rejected := []message.ContentBlock{
		message.ToolCallBlock{}, &message.ToolCallBlock{},
		message.ToolResultBlock{}, &message.ToolResultBlock{},
		message.ThinkingBlock{}, &message.ThinkingBlock{},
		message.RedactedThinkingBlock{}, &message.RedactedThinkingBlock{},
	}
	for _, b := range rejected {
		if err := ValidateContent([]message.ContentBlock{message.TextBlock{Text: "ok"}, b}); err == nil {
			t.Errorf("%T accepted", b)
		}
	}
	allowed := []message.ContentBlock{message.TextBlock{Text: "ok"}, message.DataBlock{}, message.HintBlock{}}
	if err := ValidateContent(allowed); err != nil {
		t.Errorf("text, data and hint rejected: %v", err)
	}
}
