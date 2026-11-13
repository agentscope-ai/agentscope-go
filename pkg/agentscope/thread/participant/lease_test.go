package participant

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// probeTool reports who holds its resource while it runs.
type probeTool struct {
	tool.BaseTool
	leases  *thread.Leases
	release chan struct{}
	started chan struct{}
	mu      sync.Mutex
	holders []string
}

func newProbe(leases *thread.Leases) *probeTool {
	p := &probeTool{leases: leases}
	p.ToolName = "write_file"
	p.ToolSchema = objectSchema
	p.ReadOnly = true
	return p
}

func (p *probeTool) Execute(context.Context, map[string]any) (*tool.ToolResponse, error) {
	if p.started != nil {
		p.started <- struct{}{}
	}
	if p.release != nil {
		<-p.release
	}
	h, _ := p.leases.Holder("a.go")
	p.mu.Lock()
	p.holders = append(p.holders, h)
	p.mu.Unlock()
	return tool.NewTextResponse("ok"), nil
}

func (p *probeTool) CheckPermissions(map[string]any, *permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.BehaviorDeny, Message: "probe says no"}
}

func pathOf(args map[string]any) string {
	s, _ := args["path"].(string)
	return s
}

func execText(t *testing.T, tl tool.Tool, in map[string]any) (string, message.ToolResultState) {
	t.Helper()
	r, err := tl.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return respText(r), r.State
}

func TestLeasedHoldsAndReleases(t *testing.T) {
	leases := thread.NewLeases(nil)
	p := newProbe(leases)
	w := Leased(p, leases, "cloud", time.Minute, pathOf)
	if w.Name() != "write_file" || !w.IsReadOnly() || w.CheckPermissions(nil, nil).Message != "probe says no" {
		t.Fatal("the wrapper must keep the tool's answers")
	}
	if _, ok := w.(tool.StreamingTool); ok {
		t.Fatal("a plain tool must not become streaming")
	}
	if _, ok := w.(permission.CompoundRuleMatcher); ok {
		t.Fatal("a plain tool must not become a compound matcher")
	}
	if out, st := execText(t, w, map[string]any{"path": "a.go"}); out != "ok" || st != message.ToolResultSuccess {
		t.Fatalf("run: %q %v", out, st)
	}
	if p.holders[0] != "cloud" {
		t.Fatalf("holder during the call = %q", p.holders[0])
	}
	if h, held := leases.Holder("a.go"); held {
		t.Fatalf("the lease must be released, held by %q", h)
	}

	// Another holder's lease refuses the call and names the holder.
	if _, err := leases.Acquire("a.go", "edge", time.Minute); err != nil {
		t.Fatal(err)
	}
	out, st := execText(t, w, map[string]any{"path": "a.go"})
	if st != message.ToolResultError || out != "a.go is in use by edge; wait for it to finish or work on something else" || len(p.holders) != 1 {
		t.Fatalf("conflict: %q %v", out, st)
	}
	// A call without a resource runs without a lease.
	if out, _ := execText(t, w, map[string]any{}); out != "ok" {
		t.Fatalf("no resource: %q", out)
	}
	if out, _ := execText(t, Leased(p, leases, "cloud", time.Minute, nil), map[string]any{"path": "a.go"}); out != "ok" {
		t.Fatalf("no resource function: %q", out)
	}
	// A lease the table refuses for another reason is reported too.
	bad := Leased(p, leases, "cloud", 0, pathOf)
	if out, st := execText(t, bad, map[string]any{"path": "b.go"}); st != message.ToolResultError || !strings.HasPrefix(out, "lease b.go:") {
		t.Fatalf("bad ttl: %q", out)
	}
}

func TestLeasedSharesLeaseAcrossCallsAndRenews(t *testing.T) {
	leases := thread.NewLeases(nil)
	p := newProbe(leases)
	p.started, p.release = make(chan struct{}, 2), make(chan struct{})
	ttl := 200 * time.Millisecond
	w := Leased(p, leases, "cloud", ttl, pathOf)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = w.Execute(context.Background(), map[string]any{"path": "a.go"})
		}()
	}
	<-p.started
	<-p.started
	time.Sleep(2*ttl + ttl/4) // past the first expiry: only renewal keeps it
	if h, _ := leases.Holder("a.go"); h != "cloud" {
		t.Fatalf("the lease must be renewed while the calls run, holder %q", h)
	}
	p.release <- struct{}{}
	// The second call still runs, so the first call's return keeps the lease.
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		n := len(p.holders)
		p.mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
	if h, _ := leases.Holder("a.go"); h != "cloud" {
		t.Fatalf("one call returned; the other still needs the lease, holder %q", h)
	}
	p.release <- struct{}{}
	wg.Wait()
	if _, held := leases.Holder("a.go"); held {
		t.Fatal("the last call releases the lease")
	}
}

// streamProbe is a streaming tool with compound rule matching and validators.
type streamProbe struct {
	*probeTool
	err   error
	calls []string
}

func (s *streamProbe) ExecuteStream(context.Context, map[string]any) (<-chan tool.ToolChunk, error) {
	if s.err != nil {
		return nil, s.err
	}
	ch := make(chan tool.ToolChunk, 2)
	h, _ := s.leases.Holder("a.go")
	ch <- tool.ToolChunk{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: h}}}
	ch <- tool.ToolChunk{IsFinal: true, State: message.ToolResultSuccess}
	close(ch)
	return ch, nil
}

func (s *streamProbe) MatchAllSubcommands([]string, map[string]any) bool {
	s.calls = append(s.calls, "all")
	return true
}

func (s *streamProbe) MatchAnySubcommand([]string, map[string]any) bool {
	s.calls = append(s.calls, "any")
	return false
}

func (s *streamProbe) ValidateInput(map[string]any) error { return errors.New("bad input") }

func (s *streamProbe) ValidateExternalResult(map[string]any, *message.ToolResultBlock) error {
	return errors.New("bad result")
}

func (s *streamProbe) GetMiddlewares() []tool.ToolMiddleware { return []tool.ToolMiddleware{nil} }

// compoundOnly matches compound rules but does not stream.
type compoundOnly struct {
	*probeTool
	inner *streamProbe
}

func (c *compoundOnly) MatchAllSubcommands(r []string, in map[string]any) bool {
	return c.inner.MatchAllSubcommands(r, in)
}

func (c *compoundOnly) MatchAnySubcommand(r []string, in map[string]any) bool {
	return c.inner.MatchAnySubcommand(r, in)
}

func drainChunks(t *testing.T, ch <-chan tool.ToolChunk) []tool.ToolChunk {
	t.Helper()
	var out []tool.ToolChunk
	for c := range ch {
		out = append(out, c)
	}
	return out
}

func TestLeasedStreamingAndCompound(t *testing.T) {
	leases := thread.NewLeases(nil)
	sp := &streamProbe{probeTool: newProbe(leases)}
	w := Leased(sp, leases, "cloud", time.Minute, pathOf)
	st, ok := w.(tool.StreamingTool)
	if !ok {
		t.Fatal("a streaming tool must stay streaming")
	}
	cm, ok := w.(permission.CompoundRuleMatcher)
	if !ok {
		t.Fatal("a compound matcher must stay one")
	}
	if !cm.MatchAllSubcommands(nil, nil) || cm.MatchAnySubcommand(nil, nil) || strings.Join(sp.calls, ",") != "all,any" {
		t.Fatalf("compound delegation: %v", sp.calls)
	}
	ch, err := st.ExecuteStream(context.Background(), map[string]any{"path": "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	chunks := drainChunks(t, ch)
	if len(chunks) != 2 || chunks[0].Content[0].(message.TextBlock).Text != "cloud" {
		t.Fatalf("chunks = %+v", chunks)
	}
	if _, held := leases.Holder("a.go"); held {
		t.Fatal("the lease is released when the stream closes")
	}

	if _, err := leases.Acquire("a.go", "edge", time.Minute); err != nil {
		t.Fatal(err)
	}
	ch, err = st.ExecuteStream(context.Background(), map[string]any{"path": "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	chunks = drainChunks(t, ch)
	if len(chunks) != 1 || !chunks[0].IsFinal || chunks[0].State != message.ToolResultError ||
		!strings.Contains(chunks[0].Content[0].(message.TextBlock).Text, "in use by edge") {
		t.Fatalf("refusal chunks = %+v", chunks)
	}
	_ = leases.Release("a.go", "edge")

	sp.err = errors.New("cannot start")
	if _, err := st.ExecuteStream(context.Background(), map[string]any{"path": "a.go"}); err == nil {
		t.Fatal("a start error is returned")
	}
	if _, held := leases.Holder("a.go"); held {
		t.Fatal("a failed start releases the lease")
	}

	// Validators and middlewares are delegated.
	v := w.(tool.InputValidator)
	r := w.(tool.ExternalResultValidator)
	if v.ValidateInput(nil) == nil || r.ValidateExternalResult(nil, nil) == nil {
		t.Fatal("validators must be delegated")
	}
	mw := w.(interface{ GetMiddlewares() []tool.ToolMiddleware })
	if len(mw.GetMiddlewares()) != 1 {
		t.Fatal("middlewares must be delegated")
	}

	// A compound tool that does not stream keeps only the matcher.
	co := Leased(&compoundOnly{probeTool: newProbe(leases), inner: sp}, leases, "cloud", time.Minute, pathOf)
	if _, ok := co.(tool.StreamingTool); ok {
		t.Fatal("a non-streaming tool must not become streaming")
	}
	if m, ok := co.(permission.CompoundRuleMatcher); !ok || !m.MatchAllSubcommands(nil, nil) || m.MatchAnySubcommand(nil, nil) {
		t.Fatal("compound delegation without streaming")
	}

	// A streaming tool without compound matching.
	so := Leased(&streamOnly{probeTool: newProbe(leases), s: sp}, leases, "cloud", time.Minute, pathOf)
	if _, ok := so.(permission.CompoundRuleMatcher); ok {
		t.Fatal("a non-compound tool must not become a matcher")
	}
	if _, ok := so.(tool.StreamingTool); !ok {
		t.Fatal("streaming must be kept")
	}

	// A plain tool has no validators or middlewares to delegate to.
	plain := Leased(newProbe(leases), leases, "cloud", time.Minute, pathOf)
	if plain.(tool.InputValidator).ValidateInput(nil) != nil || plain.(tool.ExternalResultValidator).ValidateExternalResult(nil, nil) != nil ||
		plain.(interface{ GetMiddlewares() []tool.ToolMiddleware }).GetMiddlewares() != nil {
		t.Fatal("a plain tool validates nothing")
	}
}

// testClock is a lease clock the test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// gatedStream streams one chunk, and a final one when final is set, once
// release closes; trailing adds a chunk after the final one.
type gatedStream struct {
	*probeTool
	final, trailing bool
}

func (g *gatedStream) ExecuteStream(context.Context, map[string]any) (<-chan tool.ToolChunk, error) {
	ch := make(chan tool.ToolChunk, 3)
	go func() {
		defer close(ch)
		if g.started != nil {
			g.started <- struct{}{}
		}
		if g.release != nil {
			<-g.release
		}
		ch <- tool.ToolChunk{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "part"}}}
		if g.final {
			ch <- tool.ToolChunk{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsFinal: true, State: message.ToolResultSuccess}
		}
		if g.trailing {
			ch <- tool.ToolChunk{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "late"}}}
		}
	}()
	return ch, nil
}

// TestLeasedStreamKeepsChunkOrder covers the held final chunk: a chunk after
// it still arrives after it, and a lease held throughout adds no note.
func TestLeasedStreamKeepsChunkOrder(t *testing.T) {
	leases := thread.NewLeases(nil)
	w := Leased(&gatedStream{probeTool: newProbe(leases), final: true, trailing: true}, leases, "cloud", time.Minute, pathOf)
	ch, err := w.(tool.StreamingTool).ExecuteStream(context.Background(), map[string]any{"path": "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range drainChunks(t, ch) {
		got = append(got, fmt.Sprintf("%s final=%v", respText(&tool.ToolResponse{Content: c.Content}), c.IsFinal))
	}
	if strings.Join(got, "; ") != "part final=false; done final=true; late final=false" {
		t.Fatalf("chunks = %v", got)
	}
}

// TestLeasedNotesALostLease covers a lease another holder took during a
// call (it expired while renewal could not keep it): renewal stops, and the
// call's result says the resource was not exclusively held throughout.
func TestLeasedNotesALostLease(t *testing.T) {
	const note = "[The lease on a.go was lost during this call: edge took it, so the resource was not exclusively held for the whole call.]"
	ttl := 20 * time.Millisecond
	// start runs one call of a leased tool over its own lease table and
	// returns, once the edge took the lease and renewal saw it, the call's
	// release function and the table.
	start := func(t *testing.T, mk func(p *probeTool) tool.Tool, call func(w tool.Tool, out chan<- []message.ContentBlock)) (chan []message.ContentBlock, *probeTool, *thread.Leases) {
		t.Helper()
		clk := &testClock{now: time.Unix(1_000_000, 0)}
		leases := thread.NewLeases(clk.Now)
		p := newProbe(leases)
		p.started, p.release = make(chan struct{}, 1), make(chan struct{})
		w := Leased(mk(p), leases, "cloud", ttl, pathOf)
		out := make(chan []message.ContentBlock, 1)
		go call(w, out)
		<-p.started
		for {
			// The call's lease expires; the edge takes it, unless a renewal
			// came first, then try again.
			clk.advance(time.Hour)
			if _, err := leases.Acquire("a.go", "edge", time.Hour); err == nil {
				break
			}
		}
		time.Sleep(5 * ttl) // several renewal ticks
		return out, p, leases
	}
	execute := func(w tool.Tool, out chan<- []message.ContentBlock) {
		r, _ := w.Execute(context.Background(), map[string]any{"path": "a.go"})
		out <- r.Content
	}
	stream := func(w tool.Tool, out chan<- []message.ContentBlock) {
		ch, _ := w.(tool.StreamingTool).ExecuteStream(context.Background(), map[string]any{"path": "a.go"})
		var blocks []message.ContentBlock
		for c := range ch {
			if c.IsFinal {
				blocks = append(blocks, c.Content...)
			}
		}
		out <- blocks
	}
	text := func(blocks []message.ContentBlock) string {
		return respText(&tool.ToolResponse{Content: blocks})
	}

	out, p, leases := start(t, func(p *probeTool) tool.Tool { return p }, execute)
	close(p.release)
	if got := text(<-out); got != "ok\n"+note {
		t.Fatalf("plain call result: %q", got)
	}
	if h, _ := leases.Holder("a.go"); h != "edge" {
		t.Fatalf("the edge keeps the lease it took, holder %q", h)
	}

	out, p, _ = start(t, func(p *probeTool) tool.Tool { return &gatedStream{probeTool: p, final: true} }, stream)
	close(p.release)
	if got := text(<-out); got != "done\n"+note {
		t.Fatalf("streaming final chunk: %q", got)
	}

	out, p, _ = start(t, func(p *probeTool) tool.Tool { return &gatedStream{probeTool: p} }, stream)
	close(p.release)
	if got := text(<-out); got != note {
		t.Fatalf("a stream without a final chunk ends with one holding the note: %q", got)
	}
}

// streamOnly streams but does not match compound rules.
type streamOnly struct {
	*probeTool
	s *streamProbe
}

func (s *streamOnly) ExecuteStream(ctx context.Context, in map[string]any) (<-chan tool.ToolChunk, error) {
	return s.s.ExecuteStream(ctx, in)
}

// TestLeasedSameHolderNeverDropsALiveCall: calls of one holder on one
// resource share the lease. A call that takes it while another call ends
// must still hold it while it runs; the release of the ending call cannot
// fall between the new call's acquire and its count.
func TestLeasedSameHolderNeverDropsALiveCall(t *testing.T) {
	leases := thread.NewLeases(nil)
	p := newProbe(leases)
	w := Leased(p, leases, "cloud", time.Minute, pathOf)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 2000 {
				if _, err := w.Execute(context.Background(), map[string]any{"path": "a.go"}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, h := range p.holders {
		if h != "cloud" {
			t.Fatalf("call %d ran while %q held a.go", i, h)
		}
	}
	if h, ok := leases.Holder("a.go"); ok {
		t.Fatalf("the last call must release the lease, held by %q", h)
	}
}

// TestLeasedStreamStopsWhenTheReaderLeaves: a consumer that stops reading a
// leased stream does not keep the lease once the call's context ends.
func TestLeasedStreamStopsWhenTheReaderLeaves(t *testing.T) {
	defer func(d time.Duration) { leaseSendWait = d }(leaseSendWait)
	leaseSendWait = 20 * time.Millisecond
	leases := thread.NewLeases(nil)
	sp := &streamProbe{probeTool: newProbe(leases)}
	w := Leased(sp, leases, "cloud", time.Minute, pathOf)
	ctx, cancel := context.WithCancel(context.Background())
	out, err := w.(tool.StreamingTool).ExecuteStream(ctx, map[string]any{"path": "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := leases.Holder("a.go"); h != "cloud" {
		t.Fatalf("holder while streaming = %q", h)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, held := leases.Holder("a.go"); !held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lease is still held after the reader left")
		}
		time.Sleep(time.Millisecond)
	}
	for range out { //nolint:revive // the channel must close
	}
}

// cancelStream sends one chunk, waits for the call's context to end and for
// done, then sends an interrupted final chunk.
type cancelStream struct {
	tool.BaseTool
	done chan struct{}
}

func (s *cancelStream) ExecuteStream(ctx context.Context, _ map[string]any) (<-chan tool.ToolChunk, error) {
	ch := make(chan tool.ToolChunk)
	go func() {
		defer close(ch)
		ch <- tool.ToolChunk{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "part"}}}
		<-ctx.Done()
		<-s.done
		ch <- tool.ToolChunk{IsFinal: true, State: message.ToolResultInterrupted,
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "canceled"}}}
	}()
	return ch, nil
}

// TestLeasedStreamKeepsTheFinalChunkAfterCancel: a reader that keeps reading
// after the call is canceled, as UnifiedAgent does, gets the wrapped tool's
// final chunk, and the lease stays held until the wrapped stream closes.
func TestLeasedStreamKeepsTheFinalChunkAfterCancel(t *testing.T) {
	for range 50 {
		leases := thread.NewLeases(nil)
		s := &cancelStream{done: make(chan struct{})}
		s.ToolName = "s"
		w := Leased(s, leases, "cloud", time.Minute, pathOf)
		ctx, cancel := context.WithCancel(context.Background())
		out, err := w.(tool.StreamingTool).ExecuteStream(ctx, map[string]any{"path": "a.go"})
		if err != nil {
			t.Fatal(err)
		}
		<-out
		cancel()
		time.Sleep(time.Millisecond)
		if h, _ := leases.Holder("a.go"); h != "cloud" {
			t.Fatalf("the lease was released while the wrapped tool still ran (holder %q)", h)
		}
		close(s.done)
		var final *tool.ToolChunk
		for c := range out {
			if c.IsFinal {
				c := c
				final = &c
			}
		}
		if final == nil || final.State != message.ToolResultInterrupted {
			t.Fatalf("final chunk = %+v", final)
		}
		if _, held := leases.Holder("a.go"); held {
			t.Fatal("the lease must be released once the stream closed")
		}
	}
}
