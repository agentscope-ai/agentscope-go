package thread

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// checkOwnership replays the log and checks that every reply came from the
// owner of its epoch, and that every append holding a reply or an owner
// change carried a guard.
func checkOwnership(t *testing.T, env *coEnv) {
	t.Helper()
	owner := ""
	log := env.log()
	for i := range log {
		e := &log[i]
		switch e.Kind {
		case KindOwnerChange:
			owner = e.Owner.To
		case KindReply:
			if e.Origin.Participant != owner {
				t.Errorf("#%d reply from %s while %s owned the foreground", e.Seq, e.Origin.Participant, owner)
			}
		}
	}
	for _, call := range env.store.appends() {
		if call.err == nil && (slices.Contains(call.kinds, KindReply) || slices.Contains(call.kinds, KindOwnerChange)) && call.guard == nil {
			t.Errorf("unguarded append of %v", call.kinds)
		}
	}
}

// checkNotStranded checks invariant 6 at idle: every task has its final
// report, no Notify report to the current owner is past its cursor or still
// waits for a foreground activation of it, no user input waits unless it is
// parked, and no user input was dropped: each one is answered (a reply,
// marker or owner change is based on it), reached a participant through an
// import or Pending, or still waits.
func checkNotStranded(t *testing.T, env *coEnv, parts map[string]*fakePart) {
	t.Helper()
	log := env.log()
	reported := map[string]bool{}
	answered := map[uint64]bool{}
	for i := range log {
		e := &log[i]
		switch e.Kind {
		case KindReport:
			// The report a run sealed and the End it carries must agree.
			sealed := e.Task != nil && strings.HasPrefix(e.DedupKey, taskReportPrefix)
			if ended := e.Task != nil && e.Task.End != "" && e.Task.End != TaskUnfinished; sealed != ended {
				t.Errorf("report #%d: sealed by a run %v, task %+v", e.Seq, sealed, e.Task)
			}
			if sealed {
				reported[e.Task.ID] = true
			}
		case KindReply, KindMarker, KindOwnerChange:
			answered[e.BasedOn] = true
		}
	}
	for i := range log {
		if e := &log[i]; e.Kind == KindAssign && !reported[e.Task.ID] {
			t.Errorf("task %s (#%d) was never run", e.Task.ID, e.Seq)
		}
	}
	owner, _ := env.c.Owner()
	if f := parts[owner]; f != nil {
		cur := f.State().Cursor
		for i := range log {
			e := &log[i]
			if e.Kind == KindReport && e.Notify && e.Addressed(owner) && e.Seq > cur {
				t.Errorf("notify report #%d to the idle owner %s is past its cursor %d", e.Seq, owner, cur)
			}
		}
	}
	imported := map[string][]string{}
	for name, f := range parts {
		imported[name] = allImported(f)
	}
	env.c.mu.Lock()
	defer env.c.mu.Unlock()
	queued := map[uint64]bool{}
	for _, in := range env.c.inputs {
		queued[in.entry.Seq] = true
		if !in.parked {
			t.Errorf("user input #%d is stranded", in.entry.Seq)
		}
	}
	if ps := env.c.parts[owner]; ps != nil {
		for seq := range ps.notify {
			if !env.c.cycle.noWake[seq] {
				t.Errorf("notify report #%d never reached a foreground activation of the idle owner %s", seq, owner)
			}
		}
	}
	for i := range log {
		e := &log[i]
		if e.Kind != KindUserInput || answered[e.Seq] || queued[e.Seq] {
			continue
		}
		reached := false
		for name := range parts {
			if containsText(imported[name], fmt.Sprintf("[#%d user]", e.Seq)) {
				reached = true
			}
		}
		if !reached {
			t.Errorf("user input #%d was dropped: nothing answered it and no participant received it", e.Seq)
		}
	}
}

// randomScript assigns, reports, reads pending input, talks, transfers and
// fails at random.
func randomScript(pick func(int) int) fakeScript {
	return func(r *fakeRun) *ActivationEnd {
		other := "cloud"
		if r.name() == "cloud" {
			other = "edge"
		}
		for i := pick(4); i > 0; i-- {
			switch pick(5) {
			case 0:
				_, _, _ = r.assign(other, "task", pick(2) == 0)
			case 1:
				_, _ = r.report("note", pick(2) == 0, other)
			case 2:
				r.pending()
			case 3:
				r.say("words ")
			case 4:
				if r.act.Foreground && pick(3) == 0 {
					_, _ = r.transfer(other)
				}
			}
		}
		if pick(10) == 0 {
			return &ActivationEnd{Reason: types.ReplyError, Err: errors.New("random failure"), ToolCalls: pick(2)}
		}
		return &ActivationEnd{Reason: types.ReplyCompleted, Text: r.text() + "done"}
	}
}

func TestCoordinatorNeverStranded(t *testing.T) {
	for seed := uint64(1); seed <= 40; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 7))
			var mu sync.Mutex
			pick := func(n int) int {
				mu.Lock()
				defer mu.Unlock()
				return rng.IntN(n)
			}
			edge, cloud := edgeCloud()
			edge.script, cloud.script = randomScript(pick), randomScript(pick)
			env := newCoEnv(t, nil, func(cfg *Config) {
				cfg.Router = cloudRouter()
				cfg.Limits.MaxWakesPerQuery = 1000
				cfg.Limits.MaxPendingInputs = 1000
			}, edge, cloud)
			for i := 0; i < 8; i++ {
				q := Query{Text: fmt.Sprintf("query %d", i)}
				if pick(4) == 0 {
					q.Explicit = "edge"
				}
				env.submit(q)
				if pick(2) == 0 {
					env.idle()
				}
			}
			env.idle()
			checkNotStranded(t, env, map[string]*fakePart{"edge": edge, "cloud": cloud})
			checkOwnership(t, env)
		})
	}
}

func TestCoordinatorConcurrentSubmits(t *testing.T) {
	edge, cloud := edgeCloud()
	env := newCoEnv(t, nil, func(cfg *Config) {
		cfg.Router = cloudRouter()
		cfg.Limits.MaxPendingInputs = 200
	}, edge, cloud)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Go(func() {
			q := Query{Text: fmt.Sprintf("q%d", i)}
			if i%5 == 0 {
				q.Explicit = "edge"
			}
			if _, err := env.c.Submit(t.Context(), q); err != nil {
				t.Errorf("Submit: %v", err)
			}
		})
	}
	wg.Wait()
	env.idle()
	checkOwnership(t, env)
	checkNotStranded(t, env, map[string]*fakePart{"edge": edge, "cloud": cloud})
	if n := len(findEntries(env.log(), KindUserInput, "")); n != 40 {
		t.Fatalf("%d user inputs", n)
	}
}
