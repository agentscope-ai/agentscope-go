package route

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
)

// StateBudget bounds the classifier state in tokens, as counted by
// Config.Count, which by default estimates four characters per token. With
// English prose the defaults keep the whole state near 5K tokens with
// Config.IncludeHistory, inside Piccolo's 8,192-token prompt; code comes
// close, and text that tokenizes more densely, such as Chinese, can exceed
// it, so set Count to the classifier's tokenizer or lower the budget for such
// text. Without IncludeHistory the state holds the query and the
// participants only.
type StateBudget struct {
	// Query bounds the query text; a longer query is cut in the middle.
	// Default 1500.
	Query int
	// Summary bounds the peer summary text, sent only with
	// Config.IncludeHistory; a longer summary is cut in the middle. Zero, the
	// default, keeps the summary as written: it was written for the edge
	// window already.
	Summary int
	// Recent bounds the recent entries, sent only with
	// Config.IncludeHistory, each counted as its JSON encoding. Default 1500.
	Recent int
	// Participants bounds the participant list, counted as its JSON
	// encoding. Default 300.
	Participants int
	// Total bounds the whole encoded state. Default 6000.
	Total int
}

// DefaultBudget returns the budget used for zero fields.
func DefaultBudget() StateBudget {
	return StateBudget{Query: 1500, Recent: 1500, Participants: 300, Total: 6000}
}

func (b StateBudget) withDefaults() (StateBudget, error) {
	if b.Query < 0 || b.Summary < 0 || b.Recent < 0 || b.Participants < 0 || b.Total < 0 {
		return StateBudget{}, configError("negative state budget")
	}
	d := DefaultBudget()
	if b.Query == 0 {
		b.Query = d.Query
	}
	if b.Recent == 0 {
		b.Recent = d.Recent
	}
	if b.Participants == 0 {
		b.Participants = d.Participants
	}
	if b.Total == 0 {
		b.Total = d.Total
	}
	if b.Query+b.Participants >= b.Total {
		return StateBudget{}, configError("the query and participant budgets (%d + %d tokens) leave no room in the total budget of %d", b.Query, b.Participants, b.Total)
	}
	return b, nil
}

// StateStats describes a built classifier state. Without
// Config.IncludeHistory the summary and recent-entry fields stay zero.
type StateStats struct {
	// Token counts of the query text, the summary text, the recent entries
	// and the participant list as encoded, and of the whole encoded state.
	QueryTokens, SummaryTokens, RecentTokens, ParticipantTokens, TotalTokens int
	// QueryCut is set when the query was cut in the middle to Budget.Query.
	QueryCut bool
	// SummaryStale is set when a stale summary was left out.
	SummaryStale bool
	// SummaryCut is set when the summary was cut in the middle to
	// Budget.Summary.
	SummaryCut bool
	// SummaryCutForTotal is set when the summary was cut in the middle so the
	// state fits Budget.Total, after every recent entry was dropped.
	SummaryCutForTotal bool
	// SummaryDropped is set when even a cut summary did not fit Budget.Total.
	SummaryDropped bool
	// RecentKept counts the entries in the state.
	RecentKept int
	// RecentFiltered counts entries of kinds the state never carries.
	RecentFiltered int
	// RecentDropped counts older entries that did not fit Budget.Recent.
	RecentDropped int
	// RecentDroppedForTotal counts entries dropped, oldest first, so the state
	// fits Budget.Total.
	RecentDroppedForTotal int
	// ToolsDropped is set when the participants' tool groups were left out to
	// fit Budget.Participants.
	ToolsDropped bool
}

// trimNote says what was removed to fit Budget.Total, "" when nothing was.
func (s *StateStats) trimNote() string {
	var parts []string
	if s.RecentDroppedForTotal > 0 {
		parts = append(parts, fmt.Sprintf("dropped %d recent entries", s.RecentDroppedForTotal))
	}
	switch {
	case s.SummaryDropped:
		parts = append(parts, "left out the summary")
	case s.SummaryCutForTotal:
		parts = append(parts, "cut the summary in the middle")
	}
	if len(parts) == 0 {
		return ""
	}
	return "the classifier state was over its total budget: " + strings.Join(parts, ", ")
}

// cutMarker replaces the middle of a cut query or summary.
const cutMarker = "…[cut]…"

// entryTextRunes bounds the text of one recent entry.
const entryTextRunes = 400

// recentKinds are the entry kinds the state carries.
var recentKinds = map[thread.Kind]bool{
	thread.KindUserInput:   true,
	thread.KindReply:       true,
	thread.KindReport:      true,
	thread.KindAssign:      true,
	thread.KindArtifact:    true,
	thread.KindOwnerChange: true,
}

type stateDoc struct {
	Query        string             `json:"query,omitempty"`
	Summary      string             `json:"summary,omitempty"`
	Recent       []stateEntry       `json:"recent"`
	Participants []stateParticipant `json:"participants"`
}

type stateEntry struct {
	Seq   uint64 `json:"seq"`
	Kind  string `json:"kind"`
	From  string `json:"from"`
	Text  string `json:"text,omitempty"`
	Tools string `json:"tools,omitempty"`
}

// queryDoc is the state without history: what a query-only classifier
// sees of a request.
type queryDoc struct {
	Query        string             `json:"query,omitempty"`
	Participants []queryParticipant `json:"participants"`
}

type queryParticipant struct {
	Name      string   `json:"name"`
	Zone      string   `json:"zone"`
	Window    int      `json:"window,omitempty"`
	Available bool     `json:"available"`
	Tools     []string `json:"tools,omitempty"`
}

type stateParticipant struct {
	Name      string   `json:"name"`
	Zone      string   `json:"zone"`
	Window    int      `json:"window,omitempty"`
	Available bool     `json:"available"`
	Lag       uint64   `json:"lag"`
	Tools     []string `json:"tools,omitempty"`
	Owner     bool     `json:"owner,omitempty"`
}

// BuildState builds the classifier state the router would send for in. By
// default it is a JSON object with the query and the participants' names,
// zones, windows, availability and tool groups. With cfg.IncludeHistory it
// also holds the peer summary, recent entries, and each participant's lag
// and whether it owns the session. It is deterministic: the same input gives
// the same bytes, whatever the order of in.Participants. It reads
// cfg.Budget, cfg.Count and cfg.IncludeHistory only, filling zero budget
// fields and a nil counter with their defaults.
//
// When a state with history is over Budget.Total, recent entries are
// dropped oldest first, then the summary is cut in the middle, then left
// out. An error means the state cannot fit even then.
func BuildState(cfg Config, in thread.RouteRequest) (json.RawMessage, StateStats, error) { //nolint:gocritic // value parameters keep the documented API
	budget, err := cfg.Budget.withDefaults()
	if err != nil {
		return nil, StateStats{}, err
	}
	c := Config{Budget: budget, Count: cfg.Count, IncludeHistory: cfg.IncludeHistory}
	if c.Count == nil {
		c.Count = defaultCount
	}
	return buildState(&c, &in)
}

// buildState expects a normalized budget and counter.
func buildState(cfg *Config, in *thread.RouteRequest) (json.RawMessage, StateStats, error) {
	var stats StateStats
	count := cfg.Count
	b := cfg.Budget
	doc := stateDoc{Recent: []stateEntry{}}

	doc.Query = in.Query
	if count(doc.Query) > b.Query {
		doc.Query, _ = cutMiddle(doc.Query, func(s string) bool { return count(s) <= b.Query })
		stats.QueryCut = true
	}
	if !cfg.IncludeHistory {
		return queryState(cfg, in, doc.Query, &stats)
	}

	if in.PeerSummary != "" {
		if in.SummaryStale {
			stats.SummaryStale = true
		} else {
			doc.Summary = in.PeerSummary
			if b.Summary > 0 && count(doc.Summary) > b.Summary {
				doc.Summary, _ = cutMiddle(doc.Summary, func(s string) bool { return count(s) <= b.Summary })
				stats.SummaryCut = true
			}
		}
	}

	recent, costs, err := recentEntries(cfg, in, &stats)
	if err != nil {
		return nil, StateStats{}, err
	}
	doc.Recent = recent

	parts, ptoks, err := participantList(in, count, b.Participants, true, &stats)
	if err != nil {
		return nil, StateStats{}, err
	}
	doc.Participants = parts

	total := func() (int, error) {
		raw, err := encode(doc)
		if err != nil {
			return 0, err
		}
		return count(string(raw)), nil
	}
	tokens, err := total()
	if err != nil {
		return nil, StateStats{}, err
	}
	for tokens > b.Total && len(doc.Recent) > 0 {
		doc.Recent = doc.Recent[1:]
		costs = costs[1:]
		stats.RecentDroppedForTotal++
		if tokens, err = total(); err != nil {
			return nil, StateStats{}, err
		}
	}
	if tokens > b.Total && doc.Summary != "" {
		full := doc.Summary
		cut, ok := cutMiddle(full, func(s string) bool {
			doc.Summary = s
			n, err := total()
			return err == nil && n <= b.Total
		})
		if ok {
			doc.Summary = cut
			stats.SummaryCutForTotal = true
		} else {
			doc.Summary = ""
			stats.SummaryDropped = true
		}
		if tokens, err = total(); err != nil {
			return nil, StateStats{}, err
		}
	}
	if tokens > b.Total {
		return nil, StateStats{}, fmt.Errorf("route: the classifier state needs %d tokens without recent entries or summary, over the total budget of %d", tokens, b.Total)
	}

	raw, err := encode(doc)
	if err != nil {
		return nil, StateStats{}, err
	}
	stats.QueryTokens = count(doc.Query)
	stats.SummaryTokens = count(doc.Summary)
	for _, n := range costs {
		stats.RecentTokens += n
	}
	stats.ParticipantTokens = ptoks
	stats.TotalTokens = tokens
	stats.RecentKept = len(doc.Recent)
	return raw, stats, nil
}

// recentEntries filters in.Recent and keeps the newest entries that fit
// Budget.Recent, in log order, with the token cost of each.
func recentEntries(cfg *Config, in *thread.RouteRequest, stats *StateStats) ([]stateEntry, []int, error) {
	var pool []stateEntry
	for i := range in.Recent {
		e := &in.Recent[i]
		if !recentKinds[e.Kind] {
			stats.RecentFiltered++
			continue
		}
		pool = append(pool, toStateEntry(e))
	}
	start, used := len(pool), 0
	costs := make([]int, len(pool))
	for i := len(pool) - 1; i >= 0; i-- {
		raw, err := encode(pool[i])
		if err != nil {
			return nil, nil, err
		}
		n := cfg.Count(string(raw))
		if used+n > cfg.Budget.Recent {
			break
		}
		used += n
		costs[i] = n
		start = i
	}
	stats.RecentDropped = start
	kept := append([]stateEntry{}, pool[start:]...)
	return kept, costs[start:], nil
}

func toStateEntry(e *thread.Entry) stateEntry {
	from := e.Origin.String()
	if e.Kind == thread.KindUserInput {
		from = "user"
	}
	var parts []string
	switch e.Kind {
	case thread.KindOwnerChange:
		if o := e.Owner; o != nil {
			line := "foreground: " + nameOrNone(o.From) + " → " + nameOrNone(o.To)
			if o.Reason != "" {
				line += ", reason " + string(o.Reason)
			}
			parts = append(parts, line)
			if o.Brief != nil {
				parts = append(parts, "brief: "+o.Brief.Render())
			}
		}
	case thread.KindArtifact:
		for _, r := range e.Refs {
			s := r.URI
			if r.Summary != "" {
				s += ": " + r.Summary
			}
			parts = append(parts, s)
		}
	}
	if t := e.Text(); t != "" {
		parts = append(parts, t)
	}
	return stateEntry{
		Seq:   e.Seq,
		Kind:  string(e.Kind),
		From:  from,
		Text:  cutEnd(strings.Join(parts, "\n"), entryTextRunes),
		Tools: toolNames(e.Digest),
	}
}

// queryState is the state without history: the query, already cut to its
// budget, and the participants without their lag or ownership.
func queryState(cfg *Config, in *thread.RouteRequest, query string, stats *StateStats) (json.RawMessage, StateStats, error) {
	full, ptoks, err := participantList(in, cfg.Count, cfg.Budget.Participants, false, stats)
	if err != nil {
		return nil, StateStats{}, err
	}
	doc := queryDoc{Query: query, Participants: withoutHistory(full)}
	raw, err := encode(doc)
	if err != nil {
		return nil, StateStats{}, err
	}
	tokens := cfg.Count(string(raw))
	if tokens > cfg.Budget.Total {
		return nil, StateStats{}, fmt.Errorf("route: the classifier state needs %d tokens, over the total budget of %d", tokens, cfg.Budget.Total)
	}
	stats.QueryTokens = cfg.Count(query)
	stats.ParticipantTokens = ptoks
	stats.TotalTokens = tokens
	return raw, *stats, nil
}

// withoutHistory drops each participant's lag and ownership.
func withoutHistory(parts []stateParticipant) []queryParticipant {
	out := make([]queryParticipant, 0, len(parts))
	for _, p := range parts {
		out = append(out, queryParticipant{Name: p.Name, Zone: p.Zone, Window: p.Window, Available: p.Available, Tools: p.Tools})
	}
	return out
}

// participantList renders every participant sorted by name, measured as it
// is sent: with lag and ownership when history is on, without them when it
// is off. When the list is over its budget the tool groups are left out; an
// error means it is over even then.
func participantList(in *thread.RouteRequest, count func(string) int, budget int, history bool, stats *StateStats) ([]stateParticipant, int, error) {
	parts := make([]stateParticipant, 0, len(in.Participants))
	for i := range in.Participants {
		p := &in.Participants[i]
		parts = append(parts, stateParticipant{
			Name:      p.Descriptor.Name,
			Zone:      string(p.Descriptor.Zone),
			Window:    p.Descriptor.Window,
			Available: p.Available,
			Lag:       p.Lag,
			Tools:     slices.Clone(p.Descriptor.ToolGroups),
			Owner:     in.Owner != "" && p.Descriptor.Name == in.Owner,
		})
	}
	slices.SortStableFunc(parts, func(a, b stateParticipant) int { return strings.Compare(a.Name, b.Name) })
	measure := func() (int, error) {
		var v any = parts
		if !history {
			v = withoutHistory(parts)
		}
		raw, err := encode(v)
		if err != nil {
			return 0, err
		}
		return count(string(raw)), nil
	}
	n, err := measure()
	if err != nil {
		return nil, 0, err
	}
	if n <= budget {
		return parts, n, nil
	}
	for i := range parts {
		parts[i].Tools = nil
	}
	stats.ToolsDropped = true
	if n, err = measure(); err != nil {
		return nil, 0, err
	}
	if n > budget {
		return nil, 0, fmt.Errorf("route: the participant list needs %d tokens, over its budget of %d", n, budget)
	}
	return parts, n, nil
}

// encode is encodeJSON; tests replace it to make the state fail to encode.
// Replacing it is safe only while no route test runs in parallel.
var encode = encodeJSON

// encodeJSON marshals v compactly without HTML escaping, so the classifier
// reads "<" rather than "<".
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("route: encode classifier state: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// cutMiddle keeps the longest head and tail of s, joined by cutMarker, for
// which fits holds. It reports false, returning the bare marker, when not even
// the marker alone fits.
func cutMiddle(s string, fits func(string) bool) (string, bool) {
	r := []rune(s)
	build := func(k int) string {
		head, tail := (k+1)/2, k/2
		return string(r[:head]) + cutMarker + string(r[len(r)-tail:])
	}
	if !fits(build(0)) {
		return cutMarker, false
	}
	lo, hi := 0, len(r)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(build(mid)) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return build(lo), true
}

// cutEnd cuts s to at most n runes, ending a cut string with "…".
func cutEnd(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func nameOrNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// toolNames lists the tools a reply used, by name in first-use order with a
// count, as "web_search ×2, read_file": the router's state keeps tool names,
// not their arguments.
func toolNames(d *thread.ToolDigest) string {
	if d == nil {
		return ""
	}
	var order []string
	count := map[string]int{}
	for _, c := range d.Calls {
		if count[c.Name] == 0 {
			order = append(order, c.Name)
		}
		count[c.Name]++
	}
	parts := make([]string, 0, len(order))
	for _, n := range order {
		if count[n] > 1 {
			n = fmt.Sprintf("%s ×%d", n, count[n])
		}
		parts = append(parts, n)
	}
	return strings.Join(parts, ", ")
}
