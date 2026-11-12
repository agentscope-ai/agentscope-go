// Package route picks which participant of a Main Thread session owns each
// user query. It implements thread.Router.
//
// EXPERIMENTAL: every exported identifier in this package may change without
// notice.
//
// The decision is deterministic code. Availability comes first, then an
// explicit choice by the user, hard tool needs and host rules. Only when all
// of them leave the decision open does the router ask a classifier (any
// classifier.DispatchClassifier: hosted Jev, a local Piccolo server through
// the Jev adapter, Bailian, OpenAI Decisions or the offline fake), and it
// uses the answer only when the classifier is confident enough. A classifier
// failure does not stop routing: the router falls back to the mode's default.
package route

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
)

// Mode says where queries go when nothing forces a choice.
type Mode string

const (
	// LocalBased makes the edge the home. A query goes to the cloud only when
	// the classifier says it needs stronger reasoning or tools only the cloud
	// has.
	LocalBased Mode = "local_based"
	// CloudBased makes the cloud the home. A query goes to the edge when the
	// cloud is offline, when it needs tools or data only on the device, or
	// when the classifier says it is simple.
	CloudBased Mode = "cloud_based"
	// Hybrid has no home. Every open query is classified. When step 5 leaves
	// the query open, it goes to the cloud whoever owns the session, and to
	// the edge only when the cloud is not a candidate. Step 5 leaves it open
	// when there is no classifier, when the router does not ask it (the state
	// cannot be built or Authorize denies the dispatch), and when it cannot
	// use the answer (the call fails or runs out of time, the answer is
	// invalid, its confidence is below the threshold, or it chose a
	// participant over MaxLag).
	Hybrid Mode = "hybrid"
)

// DefaultThreshold is the minimum classifier confidence used when
// Config.Threshold is zero. It is the repository's offline example value;
// calibrate it for each classifier.
const DefaultThreshold = 0.8

// DefaultTimeout bounds one classifier call when Config.Timeout is zero. A
// call that runs out of time counts as a classifier failure: routing falls
// through to the mode's default (in hybrid, the cloud) instead of waiting.
const DefaultTimeout = 10 * time.Second

// QuestionID is the ID of the one Choice question the router asks.
const QuestionID = "owner"

// Rule is a host rule. It returns a target and true to decide, or false to
// pass. Rules must not modify the request.
type Rule func(in thread.RouteRequest) (target string, ok bool)

// Config configures a Router.
type Config struct {
	Mode Mode
	// Edge and Cloud are the names of the edge and the cloud participant.
	Edge, Cloud string
	// Rules run in order after the hard needs. A rule that names a
	// participant that is not a candidate is ignored.
	Rules []Rule
	// Classifier is asked when the rules leave the decision open. Nil means
	// rules and defaults only.
	Classifier classifier.DispatchClassifier
	// Threshold is the minimum confidence for a classifier answer to be
	// used, in (0, 1]. Zero means DefaultThreshold.
	Threshold float64
	// Timeout bounds one classifier call; zero means DefaultTimeout and a
	// negative value means no bound. A call that runs out of time is a
	// classifier failure, so routing falls through to the mode's default.
	Timeout time.Duration
	// MaxLag stops the classifier from choosing a participant that has more
	// than MaxLag entries it has not imported yet. Zero means no limit. It
	// limits the classifier's choice only: a participant rejected for lag
	// still gets the query when it is the mode's default (the home, or the
	// cloud in hybrid), and then imports what it lacks.
	MaxLag uint64
	// Budget bounds the classifier state. Zero fields take the values of
	// DefaultBudget.
	Budget StateBudget
	// IncludeHistory adds the session's history to the classifier state:
	// the peer summary, recent entries, and each participant's lag and
	// whether it owns the session. False, the default, sends the query and
	// the participants' names, zones, windows, availability and tool groups
	// only, as a classifier that routes from the query alone sees a request.
	IncludeHistory bool
	// Count counts the tokens of a string. Nil counts (runes+3)/4.
	Count func(string) int
	// Authorize is the dispatch-time authorization passed to
	// ClassifyWithDispatch. It runs immediately before the classifier's one
	// dispatch, under the call's Timeout; an error prevents the dispatch.
	// Nil allows every dispatch.
	Authorize func(context.Context, classifier.AttemptKind) error
	// Limits narrows the classifier adapter's byte caps; zero keeps them.
	Limits classifier.DispatchLimits
}

// Router picks the owner of each query. It is immutable after New and safe
// for concurrent use when the configured rules, classifier, counter and
// authorizer are.
type Router struct {
	cfg          Config
	instructions string
}

var _ thread.Router = (*Router)(nil)

var (
	// ErrConfig is wrapped by every configuration error from New and
	// BuildState.
	ErrConfig = errors.New("route: invalid configuration")
	// ErrUnknownParticipant means a request named a participant that is not
	// in its participant list.
	ErrUnknownParticipant = errors.New("route: unknown participant")
	// ErrBadRequest means a request is malformed, such as a participant
	// listed twice.
	ErrBadRequest = errors.New("route: malformed request")
)

// New validates cfg and returns a Router. It checks the participant names,
// the mode, the threshold, the budget, the limits, the rules and the
// classifier.
func New(cfg Config) (*Router, error) { //nolint:gocritic // value parameter keeps the documented API
	c, err := normalize(cfg)
	if err != nil {
		return nil, err
	}
	return &Router{cfg: c, instructions: instructions(c.Mode, c.Edge, c.Cloud, c.IncludeHistory)}, nil
}

func configError(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrConfig}, args...)...)
}

func normalize(cfg Config) (Config, error) { //nolint:gocritic // a copy is what normalize returns
	for _, n := range []struct{ role, name string }{{"edge", cfg.Edge}, {"cloud", cfg.Cloud}} {
		if strings.TrimSpace(n.name) == "" {
			return Config{}, configError("the %s participant name is required", n.role)
		}
		if strings.Contains(n.name, "/") {
			return Config{}, configError("the %s participant name %q may not contain \"/\"", n.role, n.name)
		}
	}
	if cfg.Edge == cfg.Cloud {
		return Config{}, configError("the edge and the cloud participant are both named %q", cfg.Edge)
	}
	switch cfg.Mode {
	case LocalBased, CloudBased, Hybrid:
	default:
		return Config{}, configError("mode %q, want %q, %q or %q", cfg.Mode, LocalBased, CloudBased, Hybrid)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Threshold == 0 {
		cfg.Threshold = DefaultThreshold
	} else if math.IsNaN(cfg.Threshold) || cfg.Threshold <= 0 || cfg.Threshold > 1 {
		return Config{}, configError("threshold %v is outside (0, 1]", cfg.Threshold)
	}
	budget, err := cfg.Budget.withDefaults()
	if err != nil {
		return Config{}, err
	}
	cfg.Budget = budget
	if cfg.Limits.MaxRequestBytes < 0 || cfg.Limits.MaxResponseBytes < 0 {
		return Config{}, configError("negative classifier byte limit")
	}
	for i, rule := range cfg.Rules {
		if rule == nil {
			return Config{}, configError("rule %d is nil", i+1)
		}
	}
	cfg.Rules = slices.Clone(cfg.Rules)
	if cfg.Classifier != nil {
		if isNil(cfg.Classifier) {
			return Config{}, configError("the classifier is a nil pointer")
		}
	}
	if cfg.Count == nil {
		cfg.Count = defaultCount
	}
	return cfg, nil
}

func isNil(v any) bool {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

func defaultCount(s string) int { return (utf8.RuneCountInString(s) + 3) / 4 }

// Route implements thread.Router. It returns Decide's decision and error.
func (r *Router) Route(ctx context.Context, in thread.RouteRequest) (thread.RouteDecision, error) { //nolint:gocritic // value parameter matches thread.Router
	d, _, err := r.Decide(ctx, in)
	return d, err
}

// Decide picks the owner of a query and also returns the classifier's
// response when the classifier was called (nil otherwise), so the caller can
// log and account for it. The first step that decides wins:
//
//  1. Availability: the candidates are the available participants. No
//     candidate is an error wrapping thread.ErrNoCandidate. One candidate wins (Source hard), unless
//     the request named another participant, which is an error.
//  2. Explicit: a named candidate wins. Naming a participant that is not a
//     candidate is an error wrapping thread.ErrNotAllowed; naming one that is
//     not listed is an error wrapping ErrUnknownParticipant.
//  3. Hard needs: a needed tool group that exactly one candidate holds picks
//     it, unless needed groups point at different candidates.
//  4. Rules, in order.
//  5. The classifier, when configured. Its answer is used only when the
//     confidence reaches the threshold and the chosen participant's lag is
//     within MaxLag. A classifier failure falls through; only cancellation of
//     ctx is returned as an error.
//  6. The mode's default.
func (r *Router) Decide(ctx context.Context, in thread.RouteRequest) (thread.RouteDecision, *classifier.Response, error) { //nolint:gocritic // value parameter matches thread.Router
	if ctx == nil {
		return thread.RouteDecision{}, nil, errors.New("route: context is required")
	}
	if err := ctx.Err(); err != nil {
		return thread.RouteDecision{}, nil, fmt.Errorf("route: %w", err)
	}
	set, err := candidates(&in)
	if err != nil {
		return thread.RouteDecision{}, nil, err
	}
	if len(set.cands) == 0 {
		return thread.RouteDecision{}, nil, fmt.Errorf("%w (%s)", thread.ErrNoCandidate, set.describe())
	}
	if in.Explicit != "" {
		if err := set.checkExplicit(in.Explicit); err != nil {
			return thread.RouteDecision{}, nil, err
		}
	}
	if len(set.cands) == 1 {
		reason := set.cands[0].Descriptor.Name + " is the only candidate"
		return r.decided(set.cands[0].Descriptor.Name, thread.RouteHard, reason), nil, nil
	}
	if in.Explicit != "" {
		return r.decided(in.Explicit, thread.RouteExplicit, "named by the user"), nil, nil
	}
	if target, group, ok := hardNeed(in.NeedsGroups, set.cands); ok {
		return r.decided(target, thread.RouteHard, fmt.Sprintf("needs tool group %q, which only %s holds", group, target)), nil, nil
	}
	var notes []string
	for i, rule := range r.cfg.Rules {
		target, ok := rule(in)
		if !ok {
			continue
		}
		if set.isCandidate(target) {
			return r.decided(target, thread.RouteRule, fmt.Sprintf("rule %d", i+1)), nil, nil
		}
		notes = append(notes, fmt.Sprintf("rule %d chose %q, which is not a candidate", i+1, target))
	}
	var resp *classifier.Response
	if r.cfg.Classifier != nil {
		out := r.classify(ctx, &in, set)
		resp = out.resp
		notes = append(notes, out.notes...)
		if out.err != nil {
			d := thread.RouteDecision{Reason: strings.Join(notes, "; ")}
			d.ClassifierCalls, d.ClassifierUsage, d.UsageKnown = ClassifierUsage(resp)
			return d, resp, out.err
		}
		if out.target != "" {
			d := r.decided(out.target, thread.RouteClassifier, joinReason(out.reason, notes))
			d.Confidence = out.confidence
			d.ClassifierCalls, d.ClassifierUsage, d.UsageKnown = ClassifierUsage(resp)
			return d, resp, nil
		}
	}
	target, why := r.fallback(&in, set)
	d := r.decided(target, thread.RouteDefault, joinReason(fmt.Sprintf("default for mode %s: %s", r.cfg.Mode, why), notes))
	d.ClassifierCalls, d.ClassifierUsage, d.UsageKnown = ClassifierUsage(resp)
	return d, resp, nil
}

func joinReason(head string, notes []string) string {
	if len(notes) == 0 {
		return head
	}
	return head + "; " + strings.Join(notes, "; ")
}

func (r *Router) decided(target string, source thread.RouteSource, reason string) thread.RouteDecision {
	d := thread.RouteDecision{Target: target, Source: source, Reason: reason}
	d.ClassifierCalls, d.ClassifierUsage, d.UsageKnown = ClassifierUsage(nil)
	return d
}

type status int

const (
	statusCandidate status = iota + 1
	statusUnavailable
)

// candidateSet is the outcome of step 1.
type candidateSet struct {
	cands  []thread.RouteCandidate // in request order
	status map[string]status
}

func candidates(in *thread.RouteRequest) (*candidateSet, error) {
	set := &candidateSet{status: make(map[string]status, len(in.Participants))}
	for i := range in.Participants {
		p := &in.Participants[i]
		name := p.Descriptor.Name
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%w: participant %d has no name", ErrBadRequest, i+1)
		}
		if _, dup := set.status[name]; dup {
			return nil, fmt.Errorf("%w: participant %q is listed twice", ErrBadRequest, name)
		}
		if !p.Available {
			set.status[name] = statusUnavailable
			continue
		}
		set.status[name] = statusCandidate
		set.cands = append(set.cands, *p)
	}
	return set, nil
}

func (s *candidateSet) isCandidate(name string) bool { return s.status[name] == statusCandidate }

func (s *candidateSet) lookup(name string) *thread.RouteCandidate {
	for i := range s.cands {
		if s.cands[i].Descriptor.Name == name {
			return &s.cands[i]
		}
	}
	return nil
}

func (s *candidateSet) describe() string {
	if len(s.status) == 0 {
		return "no participants"
	}
	var parts []string
	var unavailable []string
	for name, st := range s.status {
		if st == statusUnavailable {
			unavailable = append(unavailable, name)
		}
	}
	slices.Sort(unavailable)
	if len(unavailable) > 0 {
		parts = append(parts, "unavailable: "+strings.Join(unavailable, ", "))
	}
	return strings.Join(parts, "; ")
}

func (s *candidateSet) checkExplicit(name string) error {
	switch s.status[name] {
	case statusCandidate:
		return nil
	case statusUnavailable:
		return fmt.Errorf("%w: %q is not available", thread.ErrNotAllowed, name)
	default:
		return fmt.Errorf("%w: %q", ErrUnknownParticipant, name)
	}
}

// hardNeed returns the one candidate that alone holds some needed tool
// group. It passes when no needed group is held by exactly one candidate, or
// when such groups point at different candidates.
func hardNeed(needs []string, cands []thread.RouteCandidate) (target, group string, ok bool) {
	for _, g := range needs {
		if strings.TrimSpace(g) == "" {
			continue
		}
		holder, n := "", 0
		for i := range cands {
			if slices.Contains(cands[i].Descriptor.ToolGroups, g) {
				holder = cands[i].Descriptor.Name
				n++
			}
		}
		if n != 1 {
			continue
		}
		if target != "" && target != holder {
			return "", "", false
		}
		if target == "" {
			target, group = holder, g
		}
	}
	return target, group, target != ""
}

// fallback is step 6. When none of the mode's preferred participants is a
// candidate, which only happens with participants other than the configured
// edge and cloud, it takes the first candidate by name.
func (r *Router) fallback(in *thread.RouteRequest, set *candidateSet) (target, why string) {
	type pref struct{ name, why string }
	var prefs []pref
	switch r.cfg.Mode {
	case Hybrid:
		// A query the classifier left open goes to the cloud. While the
		// cloud is a candidate, the edge takes a query that steps 1 to 4
		// leave open only when the classifier picks it.
		prefs = []pref{{r.cfg.Cloud, "the cloud"}, {r.cfg.Edge, "the cloud is not a candidate"}}
	case LocalBased:
		prefs = []pref{{r.cfg.Edge, "home is the edge"}, {r.cfg.Cloud, "the edge is not a candidate"}}
	case CloudBased:
		prefs = []pref{{r.cfg.Cloud, "home is the cloud"}, {r.cfg.Edge, "the cloud is not a candidate"}}
	}
	for _, p := range prefs {
		if p.name != "" && set.isCandidate(p.name) {
			return p.name, p.why
		}
	}
	names := make([]string, 0, len(set.cands))
	for i := range set.cands {
		names = append(names, set.cands[i].Descriptor.Name)
	}
	slices.Sort(names)
	return names[0], "first candidate by name"
}

// instructions returns the Choice question's instructions for the mode. The
// local_based and cloud_based questions say what the state holds, so they
// name the history only when it is sent. The hybrid question is the router
// arm's question from stage 0's fact-retention benchmark, word for word apart
// from the participant names, and does not describe the state.
func instructions(mode Mode, edge, cloud string, history bool) string {
	var b strings.Builder
	if mode != Hybrid {
		b.WriteString("Pick the participant that should answer the user's query. ")
		if history {
			b.WriteString("The state holds the query, the session summary, recent entries of the shared thread and the participants. ")
		} else {
			b.WriteString("The state holds the query and the participants. ")
		}
	}
	switch mode {
	case LocalBased:
		fmt.Fprintf(&b, "Choose the cloud (participant %q) only if the request needs stronger reasoning or tools only the cloud has; otherwise choose the edge (participant %q).", cloud, edge)
	case CloudBased:
		fmt.Fprintf(&b, "Choose the edge (participant %q) only if the request is simple enough for a small local model or needs tools or data only on the device; otherwise choose the cloud (participant %q).", edge, cloud)
	default:
		// Stage 0's router-arm question: it states what each side costs and
		// holds, and no rule of its own.
		fmt.Fprintf(&b, "Two assistants serve this session. Decide which one should answer the user's new query. "+
			"Prefer the local assistant (participant %q) whenever it can answer correctly, because it is cheaper and keeps data on the device. "+
			"Choose the cloud assistant (participant %q) when the answer needs something only the cloud has, or reasoning too hard for a small model.", edge, cloud)
	}
	return b.String()
}

// choiceText describes a candidate in stage 0's words, with tool calls in
// place of the benchmark's records and the host's description in place of
// its record-ID line: what its model holds in context, then the
// description.
func choiceText(d *thread.Descriptor) string {
	var head string
	switch d.Zone {
	case thread.ZoneCloud:
		head = "Large model with the whole session history in context, including the full results of every tool call it made. It handles hard reasoning well."
	case thread.ZoneEdge:
		head = "Small model with " + windowPhrase(d.Window) + ". It has the last published messages of the session, any summary, and its own notes. " +
			"It cannot see the results of tool calls the cloud made unless the summary states them."
	}
	return strings.TrimSpace(head + " " + strings.TrimSpace(d.Description))
}

// windowPhrase names a context window as stage 0 did ("an 8K window").
func windowPhrase(tokens int) string {
	var size string
	switch {
	case tokens <= 0:
		return "a small window"
	case tokens%1024 == 0:
		size = strconv.Itoa(tokens/1024) + "K"
	default:
		size = strconv.Itoa(tokens) + "-token"
	}
	article := "a"
	if strings.HasPrefix(size, "8") || strings.HasPrefix(size, "11") || strings.HasPrefix(size, "18") {
		article = "an"
	}
	return article + " " + size + " window"
}

// classifyOutcome is the result of step 5.
type classifyOutcome struct {
	target     string // "" when the answer is not used
	confidence float64
	reason     string
	notes      []string
	resp       *classifier.Response
	err        error // only cancellation of ctx
}

func (r *Router) classify(ctx context.Context, in *thread.RouteRequest, set *candidateSet) classifyOutcome {
	raw, stats, err := buildState(&r.cfg, in)
	if err != nil {
		return classifyOutcome{notes: []string{"the classifier was not asked: " + err.Error()}}
	}
	var out classifyOutcome
	if note := stats.trimNote(); note != "" {
		out.notes = append(out.notes, note)
	}
	choices := make(map[string]string, len(set.cands))
	for i := range set.cands {
		d := &set.cands[i].Descriptor
		choices[d.Name] = choiceText(d)
	}
	opID := "route"
	if in.OperationID != "" {
		opID = in.OperationID + "/route"
	}
	req := classifier.Request{
		OperationID: opID,
		State:       raw,
		Questions: map[string]classifier.Question{
			QuestionID: &classifier.ChoiceQuestion{Instructions: r.instructions, Choices: choices},
		},
	}
	cctx := ctx
	if r.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, r.cfg.Timeout)
		defer cancel()
	}
	begin := func(kind classifier.AttemptKind) error {
		if r.cfg.Authorize == nil {
			return nil
		}
		return r.cfg.Authorize(cctx, kind)
	}
	resp, err := r.cfg.Classifier.ClassifyWithDispatch(cctx, req, r.cfg.Limits, begin)
	out.resp = resp
	if ctxErr := ctx.Err(); ctxErr != nil {
		out.err = fmt.Errorf("route: classifier call canceled: %w", ctxErr)
		return out
	}
	if err != nil {
		out.notes = append(out.notes, "classifier failed: "+err.Error())
		return out
	}
	if err := classifier.ValidateResponse(req, resp); err != nil {
		out.notes = append(out.notes, "classifier answer is invalid: "+err.Error())
		return out
	}
	a, _ := resp.Answers[QuestionID].(*classifier.ChoiceAnswer)
	c := set.lookup(a.Choice)
	switch {
	case a.Confidence < r.cfg.Threshold:
		out.notes = append(out.notes, fmt.Sprintf("classifier chose %s with confidence %.2f, below the threshold %.2f", a.Choice, a.Confidence, r.cfg.Threshold))
	case r.cfg.MaxLag > 0 && c.Lag > r.cfg.MaxLag:
		out.notes = append(out.notes, fmt.Sprintf("classifier chose %s, which is %d entries behind (limit %d)", a.Choice, c.Lag, r.cfg.MaxLag))
	default:
		out.target = a.Choice
		out.confidence = a.Confidence
		out.reason = fmt.Sprintf("classifier chose %s with confidence %.2f", a.Choice, a.Confidence)
	}
	return out
}
