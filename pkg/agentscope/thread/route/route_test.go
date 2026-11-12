package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
)

func TestNewValidatesConfig(t *testing.T) {
	var nilFake *classifier.Fake
	ok := Config{Mode: Hybrid, Edge: "edge", Cloud: "cloud"}
	cases := []struct {
		name string
		edit func(c *Config)
	}{
		{"blank edge", func(c *Config) { c.Edge = " " }},
		{"blank cloud", func(c *Config) { c.Cloud = "" }},
		{"slash in name", func(c *Config) { c.Edge = "edge/master" }},
		{"same names", func(c *Config) { c.Cloud = "edge" }},
		{"empty mode", func(c *Config) { c.Mode = "" }},
		{"unknown mode", func(c *Config) { c.Mode = "edge_first" }},
		{"negative threshold", func(c *Config) { c.Threshold = -0.1 }},
		{"threshold above one", func(c *Config) { c.Threshold = 1.5 }},
		{"NaN threshold", func(c *Config) { c.Threshold = math.NaN() }},
		{"negative budget", func(c *Config) { c.Budget.Recent = -1 }},
		{"budget without room", func(c *Config) { c.Budget = StateBudget{Query: 3000, Participants: 3000, Total: 6000} }},
		{"negative request limit", func(c *Config) { c.Limits.MaxRequestBytes = -1 }},
		{"negative response limit", func(c *Config) { c.Limits.MaxResponseBytes = -1 }},
		{"nil rule", func(c *Config) { c.Rules = []Rule{nil} }},
		{"nil classifier pointer", func(c *Config) { c.Classifier = nilFake }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ok
			tc.edit(&cfg)
			if _, err := New(cfg); !errors.Is(err, ErrConfig) {
				t.Fatalf("New error = %v, want ErrConfig", err)
			}
		})
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	rules := []Rule{func(thread.RouteRequest) (string, bool) { return "", false }}
	r := newRouter(t, Config{Rules: rules, Budget: StateBudget{Total: 7000}})
	if r.cfg.Threshold != DefaultThreshold {
		t.Errorf("threshold = %v, want %v", r.cfg.Threshold, DefaultThreshold)
	}
	want := DefaultBudget()
	want.Total = 7000
	if r.cfg.Budget != want {
		t.Errorf("budget = %+v, want %+v", r.cfg.Budget, want)
	}
	if r.cfg.Count == nil || r.cfg.Count("abcde") != 2 {
		t.Error("the default counter should count (runes+3)/4")
	}
	rules[0] = nil
	if r.cfg.Rules[0] == nil {
		t.Error("New should copy the rules")
	}
}

func TestRouteMatchesDecide(t *testing.T) {
	r := newRouter(t, Config{Mode: CloudBased})
	d1, err := r.Route(context.Background(), request())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	d2, resp, err := r.Decide(context.Background(), request())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d1 != d2 || resp != nil {
		t.Fatalf("Route = %+v, Decide = %+v (resp %v)", d1, d2, resp)
	}
	if d1.Target != "cloud" || d1.Source != thread.RouteDefault || !d1.UsageKnown || d1.ClassifierCalls != 0 {
		t.Fatalf("decision = %+v", d1)
	}
}

func TestContextChecks(t *testing.T) {
	r := newRouter(t, Config{})
	//nolint:staticcheck // a nil context is the case under test
	if _, _, err := r.Decide(nil, request()); err == nil {
		t.Error("a nil context should be an error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Route(ctx, request()); !errors.Is(err, context.Canceled) {
		t.Errorf("Route on a canceled context = %v, want context.Canceled", err)
	}
}

func TestMalformedParticipants(t *testing.T) {
	r := newRouter(t, Config{})
	in := request()
	in.Participants = append(in.Participants, edgeCand(true))
	if _, err := r.Route(context.Background(), in); !errors.Is(err, ErrBadRequest) {
		t.Errorf("duplicate participant: %v, want ErrBadRequest", err)
	}
	in = request()
	in.Participants = append(in.Participants, namedCand(" ", thread.ZoneEdge, true))
	if _, err := r.Route(context.Background(), in); !errors.Is(err, ErrBadRequest) {
		t.Errorf("blank participant: %v, want ErrBadRequest", err)
	}
}

func TestNoCandidate(t *testing.T) {
	r := newRouter(t, Config{})
	cases := []struct {
		name string
		edit func(in *thread.RouteRequest)
		want string
	}{
		{"no participants", func(in *thread.RouteRequest) { in.Participants = nil }, "no participants"},
		{"all offline", func(in *thread.RouteRequest) {
			in.Participants = []thread.RouteCandidate{edgeCand(false), cloudCand(false)}
		}, "unavailable: cloud, edge"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := request()
			tc.edit(&in)
			_, err := r.Route(context.Background(), in)
			if !errors.Is(err, thread.ErrNoCandidate) {
				t.Fatalf("err = %v, want ErrNoCandidate", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestExplicit(t *testing.T) {
	r := newRouter(t, Config{Mode: LocalBased})
	t.Run("names a candidate", func(t *testing.T) {
		in := request()
		in.Explicit = "cloud"
		d, err := r.Route(context.Background(), in)
		if err != nil || d.Target != "cloud" || d.Source != thread.RouteExplicit {
			t.Fatalf("decision = %+v, err %v", d, err)
		}
	})
	t.Run("names an unavailable participant", func(t *testing.T) {
		in := request()
		in.Explicit = "cloud"
		in.Participants = []thread.RouteCandidate{edgeCand(true), cloudCand(false)}
		_, err := r.Route(context.Background(), in)
		if !errors.Is(err, thread.ErrNotAllowed) || !strings.Contains(err.Error(), "not available") {
			t.Fatalf("err = %v, want ErrNotAllowed for an unavailable participant", err)
		}
	})
	t.Run("names an unknown participant", func(t *testing.T) {
		in := request()
		in.Explicit = "phone"
		if _, err := r.Route(context.Background(), in); !errors.Is(err, ErrUnknownParticipant) {
			t.Fatalf("err = %v, want ErrUnknownParticipant", err)
		}
	})
	t.Run("names the only candidate", func(t *testing.T) {
		in := request()
		in.Explicit = "edge"
		in.Participants = []thread.RouteCandidate{edgeCand(true), cloudCand(false)}
		d, err := r.Route(context.Background(), in)
		if err != nil || d.Target != "edge" || d.Source != thread.RouteHard {
			t.Fatalf("decision = %+v, err %v", d, err)
		}
	})
}

func TestExplicitBeatsLaterSteps(t *testing.T) {
	fake := choiceFake("cloud", 0.99)
	r := newRouter(t, Config{
		Mode:       LocalBased,
		Classifier: fake,
		Rules:      []Rule{func(thread.RouteRequest) (string, bool) { return "cloud", true }},
	})
	in := request()
	in.Explicit = "edge"
	in.NeedsGroups = []string{"web"}
	d, resp, err := r.Decide(context.Background(), in)
	if err != nil || d.Target != "edge" || d.Source != thread.RouteExplicit || resp != nil {
		t.Fatalf("decision = %+v, resp %v, err %v", d, resp, err)
	}
}

func TestHardNeeds(t *testing.T) {
	// CloudBased defaults to the cloud; LocalBased to the edge. Each case
	// uses the mode whose default differs from the expected hard choice.
	cases := []struct {
		name   string
		mode   Mode
		edge   []string
		cloud  []string
		needs  []string
		want   string
		source thread.RouteSource
	}{
		{"device only on the edge", CloudBased, []string{"device"}, []string{"web"}, []string{"device"}, "edge", thread.RouteHard},
		{"web only on the cloud", LocalBased, []string{"device"}, []string{"web"}, []string{"web"}, "cloud", thread.RouteHard},
		{"group on both sides", CloudBased, []string{"files"}, []string{"files"}, []string{"files"}, "cloud", thread.RouteDefault},
		{"needs split across sides", CloudBased, []string{"device"}, []string{"web"}, []string{"device", "web"}, "cloud", thread.RouteDefault},
		{"exclusive group with a shared one", CloudBased, []string{"device", "files"}, []string{"files"}, []string{"files", "device"}, "edge", thread.RouteHard},
		{"group nobody holds", LocalBased, []string{"device"}, []string{"web"}, []string{"gpu", ""}, "edge", thread.RouteDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRouter(t, Config{Mode: tc.mode})
			in := request()
			in.Participants = []thread.RouteCandidate{edgeCand(true, tc.edge...), cloudCand(true, tc.cloud...)}
			in.NeedsGroups = tc.needs
			d, err := r.Route(context.Background(), in)
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if d.Target != tc.want || d.Source != tc.source {
				t.Fatalf("decision = %+v, want %s from %s", d, tc.want, tc.source)
			}
		})
	}
}

func TestRules(t *testing.T) {
	var seen []string
	rule := func(name, target string, ok bool) Rule {
		return func(in thread.RouteRequest) (string, bool) {
			seen = append(seen, name+":"+in.OperationID)
			return target, ok
		}
	}
	r := newRouter(t, Config{
		Mode: LocalBased,
		Rules: []Rule{
			rule("unknown", "phone", true),
			rule("pass", "cloud", false),
			rule("cloud", "cloud", true),
			rule("never", "edge", true),
		},
	})
	d, err := r.Route(context.Background(), request())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.Target != "cloud" || d.Source != thread.RouteRule || !strings.HasPrefix(d.Reason, "rule 3") {
		t.Fatalf("decision = %+v", d)
	}
	if strings.Join(seen, ",") != "unknown:op-1,pass:op-1,cloud:op-1" {
		t.Fatalf("rules ran as %v", seen)
	}

	// A rule that names an unavailable participant is ignored too.
	r = newRouter(t, Config{
		Mode:  LocalBased,
		Rules: []Rule{rule("offline", "cloud", true)},
	})
	in := request()
	in.Participants = append(in.Participants, namedCand("lab", thread.ZoneEdge, true))
	in.Participants[1].Available = false
	d, err = r.Route(context.Background(), in)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.Target != "edge" || d.Source != thread.RouteDefault || !strings.Contains(d.Reason, `rule 1 chose "cloud", which is not a candidate`) {
		t.Fatalf("decision = %+v", d)
	}
}

func TestClassifierAccept(t *testing.T) {
	var kinds []classifier.AttemptKind
	rec := &recorder{inner: choiceFake("cloud", 0.9)}
	r := newRouter(t, Config{
		Mode:       Hybrid,
		Classifier: rec,
		Limits:     classifier.DispatchLimits{MaxRequestBytes: 1 << 20},
		Authorize: func(_ context.Context, k classifier.AttemptKind) error {
			kinds = append(kinds, k)
			return nil
		},
	})
	in := request()
	in.Owner = "edge"
	d, resp, err := r.Decide(context.Background(), in)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Target != "cloud" || d.Source != thread.RouteClassifier || d.Confidence != 0.9 {
		t.Fatalf("decision = %+v", d)
	}
	if resp == nil || len(resp.Attempts) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	if d.ClassifierCalls != 1 || !d.UsageKnown || d.ClassifierUsage == nil ||
		d.ClassifierUsage.InputTokens != 120 || d.ClassifierUsage.OutputTokens != 3 {
		t.Fatalf("accounting = %d calls, %+v, known %v", d.ClassifierCalls, d.ClassifierUsage, d.UsageKnown)
	}
	if len(kinds) != 1 || kinds[0] != classifier.SimulatedAttempt {
		t.Fatalf("authorize saw %v", kinds)
	}
	req := rec.last(t)
	if req.OperationID != "op-1/route" {
		t.Errorf("operation ID = %q", req.OperationID)
	}
	if rec.limits[0].MaxRequestBytes != 1<<20 {
		t.Errorf("limits = %+v", rec.limits[0])
	}
	q, ok := req.Questions[QuestionID].(*classifier.ChoiceQuestion)
	if !ok || len(req.Questions) != 1 {
		t.Fatalf("questions = %#v", req.Questions)
	}
	if len(q.Choices) != 2 || !strings.HasPrefix(q.Choices["edge"], "Small model with an 8K window. It has the last published messages of the session, any summary, and its own notes.") ||
		!strings.HasSuffix(q.Choices["edge"], edgeDesc) || !strings.HasPrefix(q.Choices["cloud"], "Large model with the whole session history") ||
		!strings.HasSuffix(q.Choices["cloud"], cloudDesc) {
		t.Errorf("choices = %v", q.Choices)
	}
	if !strings.Contains(q.Instructions, "Prefer the local assistant") {
		t.Errorf("instructions = %q", q.Instructions)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(req.State, &state); err != nil {
		t.Fatalf("state is not a JSON object: %v", err)
	}
	if string(state["query"]) != `"What is the weather like?"` {
		t.Errorf("state query = %s", state["query"])
	}
}

func TestClassifierOperationIDWithoutRequestID(t *testing.T) {
	rec := &recorder{inner: choiceFake("cloud", 0.9)}
	r := newRouter(t, Config{Classifier: rec})
	in := request()
	in.OperationID = ""
	if _, err := r.Route(context.Background(), in); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if got := rec.last(t).OperationID; got != "route" {
		t.Fatalf("operation ID = %q, want route", got)
	}
}

func TestClassifierLowConfidence(t *testing.T) {
	r := newRouter(t, Config{Mode: Hybrid, Classifier: choiceFake("edge", 0.6)})
	// Hybrid sends a low-confidence query to the cloud, even when the edge
	// owns the session and the classifier leaned towards it.
	in := request()
	in.Owner = "edge"
	d, resp, err := r.Decide(context.Background(), in)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Target != "cloud" || d.Source != thread.RouteDefault || d.Confidence != 0 {
		t.Fatalf("decision = %+v", d)
	}
	if !strings.Contains(d.Reason, "below the threshold 0.80") || !strings.Contains(d.Reason, "default for mode hybrid: the cloud") {
		t.Fatalf("reason = %q", d.Reason)
	}
	if resp == nil || d.ClassifierCalls != 1 || !d.UsageKnown {
		t.Fatalf("accounting = %+v, resp %v", d, resp)
	}
}

func TestClassifierThresholdIsConfigurable(t *testing.T) {
	r := newRouter(t, Config{Mode: LocalBased, Classifier: choiceFake("cloud", 0.6), Threshold: 0.5})
	d, err := r.Route(context.Background(), request())
	if err != nil || d.Target != "cloud" || d.Source != thread.RouteClassifier {
		t.Fatalf("decision = %+v, err %v", d, err)
	}
}

func TestClassifierError(t *testing.T) {
	fake := classifier.NewFake(&classifier.FakeConfig{
		Response: classifier.Response{Attempts: []classifier.Attempt{{Kind: classifier.SimulatedAttempt, Usage: &classifier.Usage{InputTokens: i64(80)}}}},
		Error:    errors.New("service unavailable"),
	})
	r := newRouter(t, Config{Mode: CloudBased, Classifier: fake})
	d, resp, err := r.Decide(context.Background(), request())
	if err != nil {
		t.Fatalf("a classifier error must not be returned: %v", err)
	}
	if d.Target != "cloud" || d.Source != thread.RouteDefault || !strings.Contains(d.Reason, "classifier failed") {
		t.Fatalf("decision = %+v", d)
	}
	if resp == nil || d.ClassifierCalls != 1 || d.UsageKnown || d.ClassifierUsage.InputTokens != 80 {
		t.Fatalf("accounting = %+v (usage %+v), resp %v", d, d.ClassifierUsage, resp)
	}
}

func TestClassifierInvalidAnswer(t *testing.T) {
	t.Run("choice is not the most probable", func(t *testing.T) {
		fake := classifier.NewFake(&classifier.FakeConfig{Response: classifier.Response{
			Model: "jev-test",
			Answers: map[string]classifier.Answer{QuestionID: &classifier.ChoiceAnswer{
				Choice: "cloud", Confidence: 0.95, Probabilities: map[string]float64{"cloud": 0.2, "edge": 0.8},
			}},
		}})
		r := newRouter(t, Config{Mode: LocalBased, Classifier: fake})
		d, resp, err := r.Decide(context.Background(), request())
		if err != nil || d.Target != "edge" || d.Source != thread.RouteDefault || resp == nil || d.ClassifierCalls != 1 {
			t.Fatalf("decision = %+v, resp %v, err %v", d, resp, err)
		}
	})
	t.Run("missing answer without an error", func(t *testing.T) {
		s := &stub{endpoint: "fake://stub", resp: &classifier.Response{OperationID: "op-1/route", Model: "stub-model"}}
		r := newRouter(t, Config{Mode: LocalBased, Classifier: s})
		d, resp, err := r.Decide(context.Background(), request())
		if err != nil || d.Target != "edge" || d.Source != thread.RouteDefault || resp != s.resp {
			t.Fatalf("decision = %+v, resp %v, err %v", d, resp, err)
		}
		if !strings.Contains(d.Reason, "classifier answer is invalid") {
			t.Fatalf("reason = %q", d.Reason)
		}
	})
	t.Run("nil response without an error", func(t *testing.T) {
		r := newRouter(t, Config{Mode: CloudBased, Classifier: &stub{endpoint: "fake://stub"}})
		d, resp, err := r.Decide(context.Background(), request())
		if err != nil || d.Target != "cloud" || d.Source != thread.RouteDefault || resp != nil || d.ClassifierCalls != 0 {
			t.Fatalf("decision = %+v, resp %v, err %v", d, resp, err)
		}
	})
}

func TestClassifierCanceled(t *testing.T) {
	fake := classifier.NewFake(&classifier.FakeConfig{
		Response:      classifier.Response{Attempts: []classifier.Attempt{{Kind: classifier.SimulatedAttempt, Usage: &classifier.Usage{InputTokens: i64(10), OutputTokens: i64(0)}}}},
		WaitForCancel: true,
	})
	r := newRouter(t, Config{Classifier: fake})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-fake.Started()
		cancel()
	}()
	d, resp, err := r.Decide(ctx, request())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if d.Target != "" || resp == nil || d.ClassifierCalls != 1 || !d.UsageKnown || d.ClassifierUsage.InputTokens != 10 {
		t.Fatalf("decision = %+v, resp %v", d, resp)
	}
}

func TestAuthorizeDenied(t *testing.T) {
	fake := choiceFake("cloud", 0.99)
	denied := errors.New("over the classifier budget")
	r := newRouter(t, Config{
		Mode:       LocalBased,
		Classifier: fake,
		Authorize:  func(context.Context, classifier.AttemptKind) error { return denied },
	})
	d, resp, err := r.Decide(context.Background(), request())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Target != "edge" || d.Source != thread.RouteDefault || !strings.Contains(d.Reason, denied.Error()) {
		t.Fatalf("decision = %+v", d)
	}
	if resp == nil || len(resp.Attempts) != 0 || d.ClassifierCalls != 0 {
		t.Fatalf("a denied dispatch has no attempt: %+v", resp)
	}
	select {
	case <-fake.Started():
		t.Fatal("the fake dispatched despite the denial")
	default:
	}
}

func TestLagLimit(t *testing.T) {
	for _, tc := range []struct {
		lag    uint64
		want   string
		source thread.RouteSource
	}{
		{lag: 50, want: "edge", source: thread.RouteDefault},
		{lag: 10, want: "cloud", source: thread.RouteClassifier},
	} {
		r := newRouter(t, Config{Mode: LocalBased, Classifier: choiceFake("cloud", 0.95), MaxLag: 10})
		in := request()
		in.Participants[1].Lag = tc.lag
		d, err := r.Route(context.Background(), in)
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		if d.Target != tc.want || d.Source != tc.source {
			t.Fatalf("lag %d: decision = %+v", tc.lag, d)
		}
		if tc.source == thread.RouteDefault && !strings.Contains(d.Reason, "50 entries behind (limit 10)") {
			t.Fatalf("reason = %q", d.Reason)
		}
	}
	// Without a limit, lag does not matter.
	r := newRouter(t, Config{Mode: LocalBased, Classifier: choiceFake("cloud", 0.95)})
	in := request()
	in.Participants[1].Lag = 1000
	if d, err := r.Route(context.Background(), in); err != nil || d.Target != "cloud" {
		t.Fatalf("decision = %+v, err %v", d, err)
	}
}

// TestLagLimitHybrid: in hybrid, MaxLag limits the classifier's choice only.
// A lagging edge the classifier picks is refused, and the query goes to the
// default, the cloud. A lagging cloud the classifier picks is refused too,
// but it is the default, so it gets the query anyway.
func TestLagLimitHybrid(t *testing.T) {
	for _, tc := range []struct {
		name   string
		choice string
		lagged int
	}{
		{"lagging edge", "edge", 0},
		{"lagging cloud", "cloud", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRouter(t, Config{Mode: Hybrid, Classifier: choiceFake(tc.choice, 0.95), MaxLag: 10})
			in := request()
			in.Owner = "edge"
			in.Participants[tc.lagged].Lag = 50
			d, err := r.Route(context.Background(), in)
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if d.Target != "cloud" || d.Source != thread.RouteDefault {
				t.Fatalf("decision = %+v", d)
			}
			if !strings.Contains(d.Reason, "50 entries behind (limit 10)") || !strings.Contains(d.Reason, "default for mode hybrid: the cloud") {
				t.Fatalf("reason = %q", d.Reason)
			}
		})
	}
}

func TestModeDefaults(t *testing.T) {
	both := []thread.RouteCandidate{edgeCand(true), cloudCand(true)}
	cases := []struct {
		name  string
		mode  Mode
		owner string
		parts []thread.RouteCandidate
		want  string
		why   string
	}{
		{"hybrid keeps a cloud owner on the cloud", Hybrid, "cloud", both, "cloud", "default for mode hybrid: the cloud"},
		{"hybrid moves an edge owner to the cloud", Hybrid, "edge", both, "cloud", "default for mode hybrid: the cloud"},
		{"hybrid without owner takes the cloud", Hybrid, "", both, "cloud", "default for mode hybrid: the cloud"},
		{"hybrid owner not a candidate", Hybrid, "lab", append([]thread.RouteCandidate{namedCand("lab", thread.ZoneCloud, false)}, both...), "cloud", "default for mode hybrid: the cloud"},
		{"hybrid without cloud takes the edge", Hybrid, "cloud", []thread.RouteCandidate{edgeCand(true), cloudCand(false), namedCand("lab", thread.ZoneCloud, true)}, "edge", "the cloud is not a candidate"},
		{"hybrid first by name", Hybrid, "", []thread.RouteCandidate{edgeCand(false), cloudCand(false), namedCand("lab", thread.ZoneCloud, true), namedCand("beta", thread.ZoneEdge, true)}, "beta", "first candidate by name"},
		{"local based goes home", LocalBased, "cloud", both, "edge", "home is the edge"},
		{"local based without edge", LocalBased, "", []thread.RouteCandidate{edgeCand(false), cloudCand(true), namedCand("lab", thread.ZoneCloud, true)}, "cloud", "the edge is not a candidate"},
		{"cloud based goes home", CloudBased, "edge", both, "cloud", "home is the cloud"},
		{"cloud based without cloud", CloudBased, "", []thread.RouteCandidate{edgeCand(true), cloudCand(false), namedCand("lab", thread.ZoneEdge, true)}, "edge", "the cloud is not a candidate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRouter(t, Config{Mode: tc.mode})
			in := request()
			in.Owner = tc.owner
			in.Participants = tc.parts
			d, err := r.Route(context.Background(), in)
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if d.Target != tc.want || d.Source != thread.RouteDefault || !strings.Contains(d.Reason, tc.why) {
				t.Fatalf("decision = %+v, want %s (%s)", d, tc.want, tc.why)
			}
		})
	}
}

func TestInstructions(t *testing.T) {
	const (
		pick        = `Pick the participant that should answer the user's query. `
		withHistory = `The state holds the query, the session summary, recent entries of the shared thread and the participants. `
		queryOnly   = `The state holds the query and the participants. `
		local       = `Choose the cloud (participant "sky") only if the request needs stronger reasoning or tools only the cloud has; otherwise choose the edge (participant "phone").`
		cloud       = `Choose the edge (participant "phone") only if the request is simple enough for a small local model or needs tools or data only on the device; otherwise choose the cloud (participant "sky").`
	)
	cases := []struct {
		mode    Mode
		history bool
		want    string
	}{
		{LocalBased, false, pick + queryOnly + local},
		{LocalBased, true, pick + withHistory + local},
		{CloudBased, false, pick + queryOnly + cloud},
		{CloudBased, true, pick + withHistory + cloud},
		{Hybrid, false, stage0Hybrid},
		{Hybrid, true, stage0Hybrid},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/history=%t", tc.mode, tc.history), func(t *testing.T) {
			rec := &recorder{inner: classifier.NewFake(nil)}
			r := newRouter(t, Config{Mode: tc.mode, Edge: "phone", Cloud: "sky", Classifier: rec, IncludeHistory: tc.history})
			in := request()
			in.Participants = []thread.RouteCandidate{namedCand("phone", thread.ZoneEdge, true), namedCand("sky", thread.ZoneCloud, true)}
			if _, err := r.Route(context.Background(), in); err != nil {
				t.Fatalf("Route: %v", err)
			}
			q := rec.last(t).Questions[QuestionID].(*classifier.ChoiceQuestion)
			if q.Instructions != tc.want {
				t.Fatalf("instructions\n%q\nwant\n%q", q.Instructions, tc.want)
			}
		})
	}
}

// stage0Hybrid is stage 0's route question with the test's participant
// names.
const stage0Hybrid = `Two assistants serve this session. Decide which one should answer the user's new query. ` +
	`Prefer the local assistant (participant "phone") whenever it can answer correctly, because it is cheaper and keeps data on the device. ` +
	`Choose the cloud assistant (participant "sky") when the answer needs something only the cloud has, or reasoning too hard for a small model.`

func TestStateBuildFailureSkipsClassifier(t *testing.T) {
	rec := &recorder{inner: choiceFake("cloud", 0.99)}
	// This counter makes every whole state far too large.
	count := func(s string) int {
		if strings.Contains(s, `"participants":`) {
			return 1_000_000
		}
		return defaultCount(s)
	}
	r := newRouter(t, Config{Mode: LocalBased, Classifier: rec, Count: count})
	d, resp, err := r.Decide(context.Background(), request())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if rec.calls() != 0 || resp != nil {
		t.Fatal("the classifier should not be asked without a state")
	}
	if d.Target != "edge" || d.Source != thread.RouteDefault || !strings.Contains(d.Reason, "the classifier was not asked") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestStateTrimmingIsInReason(t *testing.T) {
	rec := &recorder{inner: choiceFake("cloud", 0.99)}
	r := newRouter(t, Config{Mode: LocalBased, Classifier: rec, IncludeHistory: true, Budget: StateBudget{Query: 50, Participants: 100, Total: 400}})
	in := request()
	in.PeerSummary = strings.Repeat("summary words ", 200)
	in.Recent = []thread.Entry{{Seq: 9, Kind: thread.KindUserInput, Content: textBlocks("hello there")}}
	d, err := r.Route(context.Background(), in)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.Source != thread.RouteClassifier || !strings.Contains(d.Reason, "cut the summary in the middle") || !strings.Contains(d.Reason, "dropped 1 recent entries") {
		t.Fatalf("decision = %+v", d)
	}
}

// hang is a classifier that answers only when its context ends.
type hang struct{}

func (hang) Classify(ctx context.Context, req classifier.Request) (*classifier.Response, error) {
	return hang{}.ClassifyWithDispatch(ctx, req, classifier.DispatchLimits{}, nil)
}

func (hang) Target() (endpoint, model string) { return "fake://hang", "hang" }

func (hang) ClassifyWithDispatch(ctx context.Context, _ classifier.Request, _ classifier.DispatchLimits, _ func(classifier.AttemptKind) error) (*classifier.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestAuthorizeRunsUnderTimeout: an authorizer that waits (for admission,
// say) runs under the classifier call's Timeout, so it cannot hold the query
// past it.
func TestAuthorizeRunsUnderTimeout(t *testing.T) {
	fake := choiceFake("cloud", 0.99)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	r := newRouter(t, Config{
		Mode:       LocalBased,
		Classifier: fake,
		Timeout:    20 * time.Millisecond,
		Authorize: func(ctx context.Context, _ classifier.AttemptKind) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
				return errors.New("released by cleanup")
			}
		},
	})
	done := make(chan struct{})
	var d thread.RouteDecision
	var err error
	go func() {
		defer close(done)
		d, _, err = r.Decide(context.Background(), request())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a waiting authorizer held the query past the timeout")
	}
	if err != nil || d.Target != "edge" || d.Source != thread.RouteDefault || !strings.Contains(d.Reason, context.DeadlineExceeded.Error()) {
		t.Fatalf("decision = %+v, err %v; want the edge by default after the deadline", d, err)
	}
	select {
	case <-fake.Started():
		t.Fatal("the fake dispatched although authorization timed out")
	default:
	}
}

// TestClassifierTimeoutFallsThrough: a classifier that never answers runs
// out of Config.Timeout, and the query goes to the hybrid default, the cloud,
// instead of waiting for it or staying with the edge owner.
func TestClassifierTimeoutFallsThrough(t *testing.T) {
	r := newRouter(t, Config{Mode: Hybrid, Classifier: hang{}, Timeout: 20 * time.Millisecond})
	req := request()
	req.Owner = "edge"
	done := make(chan struct{})
	var d thread.RouteDecision
	var err error
	go func() {
		defer close(done)
		d, _, err = r.Decide(context.Background(), req)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung classifier blocked the query")
	}
	if err != nil || d.Target != "cloud" || d.Source != thread.RouteDefault || !strings.Contains(d.Reason, "classifier failed") {
		t.Fatalf("decision = %+v, err %v; want the cloud", d, err)
	}
	if def := newRouter(t, Config{Classifier: hang{}}); def.cfg.Timeout != DefaultTimeout {
		t.Fatalf("default timeout = %v", def.cfg.Timeout)
	}
}

func TestWindowPhrase(t *testing.T) {
	for tokens, want := range map[int]string{
		0:      "a small window",
		8192:   "an 8K window",
		16384:  "a 16K window",
		11264:  "an 11K window",
		131072: "a 128K window",
		6000:   "a 6000-token window",
	} {
		if got := windowPhrase(tokens); got != want {
			t.Errorf("windowPhrase(%d) = %q, want %q", tokens, got, want)
		}
	}
}
