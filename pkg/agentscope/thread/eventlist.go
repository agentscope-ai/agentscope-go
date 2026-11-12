package thread

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Event list defaults and limits.
const (
	// defaultEventMaxRecords is the record cap when EventListOptions leaves
	// MaxRecords unset.
	defaultEventMaxRecords = 1000
	// defaultEventMaxBytes is the output byte cap when EventListOptions
	// leaves MaxBytes unset.
	defaultEventMaxBytes = 8 << 20
	// MaxEventPageRunes caps the runes one Page call returns.
	MaxEventPageRunes = 4000
	// eventFirstLineRunes bounds EventSummary.FirstLine.
	eventFirstLineRunes = 120
)

// ErrEventNotFound is returned by Page for an unknown or dropped record, or
// one from the reader's own zone.
var ErrEventNotFound = errors.New("thread: event not found")

// EventRecord is one tool output a participant produced. Records are kept
// outside the log and outside every context; a peer reads them on demand.
type EventRecord struct {
	// ID is assigned by Add: "ev-1", "ev-2", ...
	ID     string
	Origin Origin
	// Zone is the zone of the participant whose tool produced the output.
	Zone   Zone
	Tool   string
	Target string
	Output string
	Time   time.Time
}

// readableBy reports whether a reader in zone z may read the record.
func (r *EventRecord) readableBy(z Zone) bool {
	return Readable(z, r.Zone)
}

// EventSummary describes a record without its output.
type EventSummary struct {
	ID, Tool, Target string
	// FirstLine is the first non-blank line of the output, at most 120 runes.
	FirstLine string
	Origin    Origin
	// Size is the output length in runes, the unit Page uses.
	Size int
}

// EventListOptions bounds an EventList.
type EventListOptions struct {
	// MaxRecords drops the oldest records beyond this; default 1000.
	MaxRecords int
	// MaxBytes is the total output bytes kept; the oldest records are
	// dropped beyond it. Default 8 MiB. A single output larger than this is
	// cut to fit.
	MaxBytes int
}

// EventList keeps the tool outputs of participants so a peer in another
// zone can read them on demand. Nothing is pushed. It is safe for concurrent
// use.
type EventList struct {
	mu      sync.Mutex
	opts    EventListOptions
	records []eventEntry
	bytes   int
	next    int
}

type eventEntry struct {
	rec   EventRecord
	runes int
}

// NewEventList returns an empty list. Non-positive options take their
// defaults.
func NewEventList(opts EventListOptions) *EventList {
	if opts.MaxRecords <= 0 {
		opts.MaxRecords = defaultEventMaxRecords
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultEventMaxBytes
	}
	return &EventList{opts: opts}
}

// Add stores a record and returns its ID. Any caller-set ID is replaced and
// a zero Time is set to the current time. The oldest records are dropped
// when the list exceeds its caps.
func (l *EventList) Add(r EventRecord) string { //nolint:gocritic // value parameter keeps the documented API
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(r.Output) > l.opts.MaxBytes {
		r.Output = cutBytes(r.Output, l.opts.MaxBytes)
	}
	l.next++
	r.ID = fmt.Sprintf("ev-%d", l.next)
	l.records = append(l.records, eventEntry{rec: r, runes: utf8.RuneCountInString(r.Output)})
	l.bytes += len(r.Output)
	drop := 0
	for len(l.records)-drop > l.opts.MaxRecords || l.bytes > l.opts.MaxBytes {
		l.bytes -= len(l.records[drop].rec.Output)
		drop++
	}
	if drop > 0 {
		clear(l.records[:drop])
		l.records = l.records[drop:]
	}
	return r.ID
}

// cutBytes returns the longest prefix of s within n bytes that ends on a
// rune boundary.
func cutBytes(s string, n int) string {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Readable reports whether a reader in zone z is offered records produced in
// zone from: records from the other zone are, records from the reader's own
// zone are not (the reader has them).
func Readable(z, from Zone) bool {
	return (z == ZoneEdge && from == ZoneCloud) || (z == ZoneCloud && from == ZoneEdge)
}

// Outline lists the records the reader may read, newest last. A positive
// limit keeps only the newest limit records.
func (l *EventList) Outline(reader Zone, limit int) []EventSummary {
	return l.list(reader, limit, func(*EventRecord) bool { return true })
}

// Search lists the readable records whose tool, target or output contains
// query, ignoring case, newest last. A positive limit keeps only the newest
// limit matches. An empty query matches every readable record.
func (l *EventList) Search(reader Zone, query string, limit int) []EventSummary {
	q := strings.ToLower(query)
	return l.list(reader, limit, func(r *EventRecord) bool {
		return strings.Contains(strings.ToLower(r.Tool), q) ||
			strings.Contains(strings.ToLower(r.Target), q) ||
			strings.Contains(strings.ToLower(r.Output), q)
	})
}

func (l *EventList) list(reader Zone, limit int, match func(*EventRecord) bool) []EventSummary {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []EventSummary
	for i := len(l.records) - 1; i >= 0; i-- {
		if limit > 0 && len(out) >= limit {
			break
		}
		e := &l.records[i]
		if !e.rec.readableBy(reader) || !match(&e.rec) {
			continue
		}
		out = append(out, EventSummary{
			ID:        e.rec.ID,
			Tool:      e.rec.Tool,
			Target:    e.rec.Target,
			FirstLine: truncateRunes(firstLine(e.rec.Output), eventFirstLineRunes),
			Origin:    e.rec.Origin,
			Size:      e.runes,
		})
	}
	// Collected newest first; return newest last.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Page returns limit runes of a readable record's output from offset, with
// the output's total length in runes. A non-positive limit, or one above
// MaxEventPageRunes, reads MaxEventPageRunes runes; a negative offset reads
// from the start. An unknown or dropped record, or one from the reader's own
// zone, is ErrEventNotFound.
func (l *EventList) Page(reader Zone, id string, offset, limit int) (text string, total int, err error) {
	l.mu.Lock()
	var output string
	found := false
	for i := range l.records {
		r := &l.records[i].rec
		if r.ID == id {
			if r.readableBy(reader) {
				output, found = r.Output, true
			}
			break
		}
	}
	l.mu.Unlock()
	if !found {
		return "", 0, fmt.Errorf("%w: %q", ErrEventNotFound, id)
	}
	if limit <= 0 || limit > MaxEventPageRunes {
		limit = MaxEventPageRunes
	}
	offset = max(offset, 0)
	runes := []rune(output)
	total = len(runes)
	if offset >= total {
		return "", total, nil
	}
	end := min(offset+limit, total)
	return string(runes[offset:end]), total, nil
}
