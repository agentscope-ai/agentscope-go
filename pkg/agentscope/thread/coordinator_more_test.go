package thread

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

func TestEndReason(t *testing.T) {
	for _, tc := range []struct {
		end  ActivationEnd
		want types.ReplyFinishedReason
	}{
		{ActivationEnd{Reason: types.ReplyExceedMaxIters}, types.ReplyExceedMaxIters},
		{ActivationEnd{Err: errors.New("x")}, types.ReplyError},
		{ActivationEnd{}, types.ReplyCompleted},
		{ActivationEnd{Reason: "odd"}, types.ReplyCompleted},
	} {
		if got := endReason(&tc.end); got != tc.want {
			t.Errorf("endReason(%+v) = %s, want %s", tc.end, got, tc.want)
		}
	}
}

func TestCoordinatorTaskFailureReports(t *testing.T) {
	edge, cloud := edgeCloud()
	cloud.push(func(r *fakeRun) *ActivationEnd {
		for i := 1; i <= 5; i++ {
			if _, _, err := r.assign("edge", fmt.Sprintf("job %d", i), false); err != nil {
				t.Errorf("assign: %v", err)
			}
		}
		return r.reply("queued five jobs")
	})
	edge.push(
		func(*fakeRun) *ActivationEnd {
			return &ActivationEnd{Reason: types.ReplyError, Err: errors.New("boom"), Text: "half", ToolCalls: 1}
		},
		func(*fakeRun) *ActivationEnd { return &ActivationEnd{Reason: types.ReplyInterrupted} },
		func(*fakeRun) *ActivationEnd { return &ActivationEnd{Reason: types.ReplyError} },
		func(*fakeRun) *ActivationEnd { return &ActivationEnd{Reason: types.ReplyCompleted} },
		func(*fakeRun) *ActivationEnd {
			return &ActivationEnd{Reason: types.ReplyExceedMaxIters, Text: "most of it"}
		},
	)
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Router = cloudRouter() }, edge, cloud)
	env.say("go")
	env.idle()
	log := env.log()
	var got []string
	for _, e := range findEntries(log, KindReport, "") {
		got = append(got, e.Text())
	}
	want := []string{
		"Task t-1 failed: boom\nPartial output:\nhalf",
		"Task t-2 failed: it was interrupted",
		"Task t-3 failed: it ended with an error",
		"Task t-4 failed: it ended with no answer",
		"most of it",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reports = %q", got)
	}
	if m := findEntries(log, KindMarker, MarkerUnknownEffects); len(m) != 1 || m[0].BasedOn != findEntries(log, KindAssign, "")[0].Seq {
		t.Fatalf("unknown_effects = %+v", m)
	}
}

// A failed task keeps its error text when it does not leave the device: a
// cloud task reporting to an edge assigner, or an edge task reporting to
// another edge participant.
func TestCoordinatorTaskFailureErrorText(t *testing.T) {
	edge, cloud := edgeCloud()
	phone := newFakePart("phone", ZoneEdge)
	edge.push(func(r *fakeRun) *ActivationEnd {
		for _, to := range []string{"cloud", "phone"} {
			if _, _, err := r.assign(to, "job", false); err != nil {
				t.Errorf("assign to %s: %v", to, err)
			}
		}
		return r.reply("asked")
	})
	cloud.push(func(*fakeRun) *ActivationEnd {
		return &ActivationEnd{Reason: types.ReplyError, Err: errors.New("quota exceeded")}
	})
	phone.push(func(*fakeRun) *ActivationEnd {
		return &ActivationEnd{Reason: types.ReplyError, Err: errors.New("camera busy")}
	})
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Summaries.PeerTarget = "edge" }, edge, cloud, phone)
	env.submit(Query{Text: "go", Explicit: "edge"})
	env.idle()
	var got []string
	for _, e := range findEntries(env.log(), KindReport, "") {
		got = append(got, e.Text())
	}
	slices.Sort(got)
	if want := []string{"Task t-1 failed: quota exceeded", "Task t-2 failed: camera busy"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reports = %q, want %q", got, want)
	}
}

func TestCoordinatorTaskOnUnavailableParticipant(t *testing.T) {
	edge, cloud := edgeCloud()
	var edgeCalls atomic.Int32
	cloud.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("edge", "job", true); err != nil {
			t.Errorf("assign: %v", err)
		}
		return r.reply("queued")
	}, replyWith("the job failed"))
	env := newCoEnv(t, nil, func(cfg *Config) {
		cfg.Router = cloudRouter()
		// Up while routing and while assigning, down when the task starts.
		cfg.Available = func(name string) bool { return name != "edge" || edgeCalls.Add(1) <= 2 }
	}, edge, cloud)
	env.say("go")
	env.idle()
	reps := findEntries(env.log(), KindReport, "")
	if len(reps) != 1 || reps[0].Text() != "Task t-1 failed: the participant is unavailable" || !reps[0].Notify {
		t.Fatalf("reports = %+v", reps)
	}
	if n := len(edge.activations()); n != 0 {
		t.Fatalf("edge ran %d times", n)
	}
	if n := len(cloud.activations()); n != 2 {
		t.Fatalf("cloud was not woken by the failure: %d activations", n)
	}
}

func TestCoordinatorBackpressureKeepsOrder(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.push(func(r *fakeRun) *ActivationEnd {
		for i := 0; i < 20; i++ {
			r.say(fmt.Sprintf("%02d ", i))
		}
		return &ActivationEnd{Reason: types.ReplyCompleted, Text: r.text()}
	})
	c, err := NewCoordinator(context.Background(), Config{
		Store: NewMemoryStore(), Participants: []Participant{edge, cloud}, Limits: Limits{EventBuffer: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Submit(context.Background(), Query{Text: "count"}); err != nil {
		t.Fatal(err)
	}
	var deltas []string
	for ev := range c.Events() {
		if ev.Kind == EventAgent {
			deltas = append(deltas, ev.Agent.(event.TextBlockDeltaEvent).Delta)
		}
		if ev.Kind == EventEntry && ev.Entry.Kind == KindReply {
			break
		}
	}
	if len(deltas) != 20 || deltas[0] != "00 " || deltas[19] != "19 " {
		t.Fatalf("deltas = %q", deltas)
	}
}

func TestCoordinatorHostOutsideActivation(t *testing.T) {
	edge, cloud := edgeCloud()
	env := newCoEnv(t, nil, nil, edge, cloud)
	env.say("hi")
	env.idle()
	h := edge.host
	ctx := context.Background()
	if s, err := h.Pending(ctx, "edge", 0); s != nil || err != nil {
		t.Fatalf("Pending while idle = %+v %v", s, err)
	}
	seq, err := h.Report(ctx, ReportRequest{From: Origin{Participant: "edge"}, Text: "later", To: []string{"cloud"}})
	if err != nil {
		t.Fatal(err)
	}
	if e, _, _ := env.store.Get(ctx, seq); e.BasedOn != 0 {
		t.Fatalf("report outside an activation based on #%d", e.BasedOn)
	}
	if _, err := h.Transfer(ctx, TransferRequest{From: Origin{Participant: "edge"}, Epoch: 1, To: "cloud", Brief: testBrief()}); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("Transfer outside an activation = %v", err)
	}

	if err := env.c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Transfer(ctx, TransferRequest{From: Origin{Participant: "edge"}, Epoch: 1, To: "cloud", Brief: testBrief()}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Transfer after Close = %v", err)
	}
	if _, _, err := h.Assign(ctx, AssignRequest{From: Origin{Participant: "edge"}, To: "cloud", Text: "x"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Assign after Close = %v", err)
	}
	if _, err := h.Report(ctx, ReportRequest{From: Origin{Participant: "edge"}, Text: "x"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Report after Close = %v", err)
	}
	if _, err := h.Publish(ctx, PublishRequest{From: Origin{Participant: "edge"}, Refs: []ArtifactRef{{URI: "x"}}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish after Close = %v", err)
	}
}

func TestCoordinatorInterruptDuringImport(t *testing.T) {
	edge := smallEdge()
	_, cloud := edgeCloud()
	env := newCoEnv(t, chunkStore(t, 6), nil, edge, cloud)
	edge.mu.Lock()
	edge.onCompact = env.c.Interrupt // the user interrupts while the import compacts
	edge.mu.Unlock()
	q := env.submit(Query{Text: "go", Explicit: "edge"})
	env.idle()
	if n := len(edge.activations()); n != 0 {
		t.Fatalf("the interrupted activation started a reply (%d activations)", n)
	}
	m := findEntries(env.log(), KindMarker, MarkerInterrupted)
	if len(m) != 1 || m[0].BasedOn != q {
		t.Fatalf("interrupted markers = %+v", m)
	}
	if strings.Contains(strings.Join(kindsOf(env.log()), ","), "reply") {
		t.Fatal("an interrupted import wrote a reply")
	}
}
