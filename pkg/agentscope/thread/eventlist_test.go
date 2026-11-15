package thread

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func ids(s []EventSummary) []string {
	out := make([]string, 0, len(s))
	for _, e := range s {
		out = append(out, e.ID)
	}
	return out
}

func cloudRecord(tool, target, output string) EventRecord {
	return EventRecord{Origin: Origin{Participant: "cloud", Path: "master"}, Zone: ZoneCloud, Tool: tool, Target: target, Output: output}
}

func edgeRecord(tool, target, output string) EventRecord {
	return EventRecord{Origin: Origin{Participant: "edge"}, Zone: ZoneEdge, Tool: tool, Target: target, Output: output}
}

func TestReadable(t *testing.T) {
	cases := []struct {
		reader, from Zone
		want         bool
	}{
		{ZoneEdge, ZoneCloud, true},
		{ZoneCloud, ZoneEdge, true},
		{ZoneEdge, ZoneEdge, false},
		{ZoneCloud, ZoneCloud, false},
		{"", ZoneCloud, false},
		{ZoneEdge, "", false},
	}
	for _, tc := range cases {
		if got := Readable(tc.reader, tc.from); got != tc.want {
			t.Errorf("Readable(%q, %q) = %v, want %v", tc.reader, tc.from, got, tc.want)
		}
	}
}

func TestEventListZones(t *testing.T) {
	l := NewEventList(EventListOptions{})
	r := cloudRecord("Read", "a.go", "\n\n  package main\nfunc main() {}")
	r.ID = "caller-set"
	if id := l.Add(r); id != "ev-1" {
		t.Fatalf("first id %q, want ev-1", id)
	}
	if id := l.Add(edgeRecord("Camera", "front", "jpeg 12KB")); id != "ev-2" {
		t.Fatalf("second id %q, want ev-2", id)
	}

	edge := l.Outline(ZoneEdge, 0)
	want := []EventSummary{{
		ID: "ev-1", Tool: "Read", Target: "a.go", FirstLine: "package main",
		Origin: Origin{Participant: "cloud", Path: "master"}, Size: utf8.RuneCountInString(r.Output),
	}}
	if !reflect.DeepEqual(edge, want) {
		t.Fatalf("edge outline %+v, want %+v", edge, want)
	}
	if got := ids(l.Outline(ZoneCloud, 0)); !reflect.DeepEqual(got, []string{"ev-2"}) {
		t.Errorf("cloud outline: %v", got)
	}
	if _, _, err := l.Page(ZoneEdge, "ev-2", 0, 0); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("edge paged its own zone's record: %v", err)
	}
	if text, total, err := l.Page(ZoneCloud, "ev-2", 0, 0); err != nil || text != "jpeg 12KB" || total != 9 {
		t.Errorf("cloud page: %q %d %v", text, total, err)
	}
}

func TestEventListCaps(t *testing.T) {
	l := NewEventList(EventListOptions{MaxRecords: 3})
	for i := 1; i <= 5; i++ {
		l.Add(cloudRecord("T", "", fmt.Sprint(i)))
	}
	if got := ids(l.Outline(ZoneEdge, 0)); !reflect.DeepEqual(got, []string{"ev-3", "ev-4", "ev-5"}) {
		t.Fatalf("record cap kept %v", got)
	}
	if _, _, err := l.Page(ZoneEdge, "ev-1", 0, 0); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("dropped record paged: %v", err)
	}

	b := NewEventList(EventListOptions{MaxBytes: 10})
	b.Add(cloudRecord("T", "", "aaaa"))
	b.Add(cloudRecord("T", "", "bbbb"))
	b.Add(cloudRecord("T", "", "cccc"))
	if got := ids(b.Outline(ZoneEdge, 0)); !reflect.DeepEqual(got, []string{"ev-2", "ev-3"}) {
		t.Fatalf("byte cap kept %v", got)
	}
	// One output above the cap is cut on a rune boundary and replaces the rest.
	id := b.Add(cloudRecord("T", "", "ééééééé")) // 14 bytes
	got := b.Outline(ZoneEdge, 0)
	if !reflect.DeepEqual(ids(got), []string{id}) {
		t.Fatalf("after an oversize record kept %v", ids(got))
	}
	text, total, err := b.Page(ZoneEdge, id, 0, 0)
	if err != nil || text != "ééééé" || total != 5 || !utf8.ValidString(text) {
		t.Errorf("oversize record page %q %d %v", text, total, err)
	}

	d := NewEventList(EventListOptions{})
	if d.opts.MaxRecords != 1000 || d.opts.MaxBytes != 8<<20 {
		t.Errorf("defaults %+v", d.opts)
	}
	if cutBytes("abc", 0) != "" || cutBytes("éa", 1) != "" || cutBytes("aé", 2) != "a" {
		t.Error("cutBytes splits a rune")
	}
}

func TestEventListOutlineAndSearch(t *testing.T) {
	l := NewEventList(EventListOptions{})
	l.Add(cloudRecord("Read", "Config.yaml", "port: 80"))
	l.Add(cloudRecord("Grep", "src", "main.go:3: TODO fix"))
	l.Add(edgeRecord("Read", "secret.txt", "TODO private"))
	l.Add(cloudRecord("WebSearch", "weather", "Sunny"))
	l.Add(cloudRecord("Bash", "go", "ok\nPASS"))

	if got := ids(l.Outline(ZoneEdge, 2)); !reflect.DeepEqual(got, []string{"ev-4", "ev-5"}) {
		t.Errorf("outline limit kept %v, want the newest two, newest last", got)
	}
	cases := []struct {
		query string
		limit int
		want  []string
	}{
		{"todo", 0, []string{"ev-2"}},
		{"READ", 0, []string{"ev-1"}},
		{"config", 0, []string{"ev-1"}},
		{"sunny", 0, []string{"ev-4"}},
		{"pass", 0, []string{"ev-5"}},
		{"o", 0, []string{"ev-1", "ev-2", "ev-5"}},
		{"o", 2, []string{"ev-2", "ev-5"}},
		{"", 0, []string{"ev-1", "ev-2", "ev-4", "ev-5"}},
		{"nothing", 0, nil},
	}
	for _, tc := range cases {
		got := ids(l.Search(ZoneEdge, tc.query, tc.limit))
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Search(%q, %d) = %v, want %v", tc.query, tc.limit, got, tc.want)
		}
	}
	if got := ids(l.Search(ZoneCloud, "todo", 0)); !reflect.DeepEqual(got, []string{"ev-3"}) {
		t.Errorf("cloud search: %v", got)
	}
	long := NewEventList(EventListOptions{})
	long.Add(cloudRecord("T", "", strings.Repeat("z", 300)))
	if fl := long.Outline(ZoneEdge, 0)[0].FirstLine; utf8.RuneCountInString(fl) != 120 || !strings.HasSuffix(fl, "…") {
		t.Errorf("first line %q not cut to 120 runes", fl)
	}
}

func TestEventListPage(t *testing.T) {
	l := NewEventList(EventListOptions{})
	out := strings.Repeat("日", 5000) + "end"
	id := l.Add(cloudRecord("T", "", out))
	total := utf8.RuneCountInString(out)
	cases := []struct {
		name          string
		offset, limit int
		want          string
	}{
		{"start", 0, 3, "日日日"},
		{"middle", 4999, 2, "日e"},
		{"end", total - 3, 10, "end"},
		{"past the end", total, 10, ""},
		{"negative offset", -5, 2, "日日"},
		{"default limit", 0, 0, strings.Repeat("日", MaxEventPageRunes)},
		{"limit capped", 0, 10000, strings.Repeat("日", MaxEventPageRunes)},
	}
	for _, tc := range cases {
		text, n, err := l.Page(ZoneEdge, id, tc.offset, tc.limit)
		if err != nil || text != tc.want || n != total {
			t.Errorf("%s: %d runes, total %d, err %v; want %d runes, total %d",
				tc.name, utf8.RuneCountInString(text), n, err, utf8.RuneCountInString(tc.want), total)
		}
	}
	if _, _, err := l.Page(ZoneEdge, "ev-404", 0, 0); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("unknown id: %v", err)
	}
}
