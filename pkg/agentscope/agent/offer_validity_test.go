package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// When a summary offer or a background precompression result still applies.
//
// Every assistant message of one reply carries the reply ID (saveToContext),
// so a reply split by boundary input leaves two context messages with one ID.
// When a peer summary then removes thread entries before a boundary, the later
// message can slide onto the boundary index with the boundary's ID. The first
// tests pin that such a shift invalidates offers and background results, and
// that changes keeping the prefix do not.

// TestOfferRejectedWhenARemovalShiftsItsBoundary drives the case through the
// public seams: an offer covering [input, A1] and keeping A1, then an
// imported entry, a second assistant message A2 of the same reply, and a peer
// summary covering the input and the entry. The context is then [A1, A2], and
// the offer must not be applied: it would replace the context with [A2] and
// lose A1, which the summary does not cover.
func TestOfferRejectedWhenARemovalShiftsItsBoundary(t *testing.T) {
	echo := tool.NewFunctionTool("echo", "e", nil, func(context.Context, map[string]any) (any, error) { return "echoed", nil })
	m := &seamModel{tokens: charTokens, respond: func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		switch call {
		case 0:
			r := seamToolCalls(message.ToolCallBlock{ID: "c1", Name: "echo"})
			r.Content = append([]message.ContentBlock{message.TextBlock{Type: "text", Text: "step one"}}, r.Content...)
			return r, nil
		case 1:
			return seamToolCalls(message.ToolCallBlock{ID: "c2", Name: "echo"}), nil
		case 2:
			return seamText("done"), nil
		}
		return summaryToolCallResponse(nil), nil // the synchronous compaction
	}}
	const offered = "summary of the reply input"
	var a *UnifiedAgent
	var offerErr error
	boundary := 0
	src := func(context.Context) (*ThreadUpdate, error) {
		boundary++
		switch boundary {
		case 2: // after the first tool step: offer notes on [input, A1], keeping A1
			snap, err := a.ForkSnapshot()
			if err != nil {
				return nil, err
			}
			offerErr = a.OfferSummary(context.Background(), snap, offered,
				SummaryApplyOptions{MinTailTokens: 1, MinTailTextMessages: 1, MaxTailTokens: 100000})
			return &ThreadUpdate{Messages: []*message.Msg{importedMsg(2, "edge note")}, Cursor: 2}, nil
		case 3: // after the second: a peer summary covers the input and the entry
			return &ThreadUpdate{PeerSummary: &PeerSummary{Text: "peer notes", Origin: "cloud", Through: 2}}, nil
		}
		return nil, nil
	}
	a = NewUnifiedAgent("a", "sys", m, WithToolkit(tool.NewToolkit(echo)), WithBoundaryInput(src), WithForkCapture(),
		WithContextConfig(&ContextConfig{ContextSize: 100000}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.Reply(WithThreadInput(ctx, 1), "the reply input"); err != nil {
		t.Fatal(err)
	}
	if offerErr != nil {
		t.Fatal(offerErr)
	}
	a.mu.Lock()
	first := a.state.Context[0]
	shifted := len(a.state.Context) == 2 && a.state.Context[1].ID == first.ID && a.state.SummaryOffer != nil &&
		a.state.SummaryOffer.Boundary == first.ID
	a.mu.Unlock()
	if !shifted {
		t.Fatal("setup: want [A1, A2] sharing the reply ID, with the offer's boundary on A1")
	}

	if _, err := a.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.Summary == offered {
		t.Fatal("the offer was applied over a shifted boundary")
	}
	if m.callCount() != 4 {
		t.Fatalf("model calls = %d, want 4: the compaction must run synchronously", m.callCount())
	}
	if a.state.SummaryOffer != nil || a.pendingPrecompress != nil {
		t.Fatal("the stale offer must be dropped")
	}
}

// TestBackgroundResultRejectedWhenARemovalShiftsItsBoundary: a generation in
// flight while the removal happens finishes with a snapshot [input, A1]. The
// live context is then [A1, A2], with A2 carrying A1's ID at the snapshot's
// boundary index; the result must not be applied.
func TestBackgroundResultRejectedWhenARemovalShiftsItsBoundary(t *testing.T) {
	m := &gatedPrecompressModel{entered: make(chan struct{}), gate: make(chan struct{})}
	m.contextSize = 1000
	m.perMessageTokens = 200 // three input messages: past the precompress line, under the trigger
	a := NewUnifiedAgent("t", "sys", m, WithContextConfig(&ContextConfig{
		ContextSize: 1000, TriggerRatio: 0.8, ReserveRatio: 0.3, PrecompressRatio: 0.5,
	}))
	assistant := func(text string) *message.Msg {
		msg := message.AssistantMsg("t", text)
		msg.ID = "reply-1"
		return msg
	}
	first := assistant("step one")
	a.state.Context = []*message.Msg{importedMsg(1, "the reply input"), first}
	if err := a.compressContext(context.Background()); err != nil {
		t.Fatalf("compressContext: %v", err)
	}
	select {
	case <-m.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the background generation did not start")
	}
	a.mu.Lock()
	a.state.Context = append(a.state.Context, importedMsg(2, "edge note"), assistant("step two"))
	a.mu.Unlock()
	if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{PeerSummary: &PeerSummary{Text: "peer notes", Through: 2}}); err != nil {
		t.Fatal(err)
	}
	close(m.gate)
	if p := waitPrecompress(t, a); p == nil {
		t.Fatal("setup: the background generation stored no result")
	}

	if a.applyReadyPrecompress(context.Background()) {
		t.Fatal("a background result was applied over a shifted boundary")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.state.Context) != 2 || a.state.Context[0] != first || a.state.Summary != "" {
		t.Fatalf("state changed: summary %q, %d messages", a.state.Summary, len(a.state.Context))
	}
}

// TestOfferStaysValidAcrossChangesThatKeepItsPrefix: the digest must not
// reject what the boundary check accepted before it. An in-place clone of the
// boundary message, appends past it, a removal past it and a reload (where a
// thread seq comes back as a float64) all keep an offer valid.
func TestOfferStaysValidAcrossChangesThatKeepItsPrefix(t *testing.T) {
	opt := SummaryApplyOptions{MinTailTokens: 1, MinTailTextMessages: 1, MaxTailTokens: 100000}
	offer := func(t *testing.T, prefix ...*message.Msg) *UnifiedAgent {
		t.Helper()
		a := NewUnifiedAgent("edge", "sys", &seamModel{tokens: charTokens}, WithForkCapture())
		a.state.Context = append([]*message.Msg(nil), prefix...)
		a.captureForkRequest(&middleware.ModelCallInput{Messages: a.prepareModelInput(context.Background())})
		snap, err := a.ForkSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		if err := a.OfferSummary(context.Background(), snap, "the notes", opt); err != nil {
			t.Fatal(err)
		}
		return a
	}
	own := func(text string) *message.Msg {
		m := message.AssistantMsg("edge", text)
		m.ID = "reply-1"
		return m
	}

	t.Run("clone, appends and a removal past the boundary", func(t *testing.T) {
		a := offer(t, message.UserMsg("u", "the input"), own("step one"))
		a.mu.Lock()
		a.state.Context[1] = cloneMsgs(a.state.Context[1:2])[0]
		a.state.Context = append(a.state.Context, importedMsg(5, "late entry"), own("step two"))
		a.mu.Unlock()
		if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{PeerSummary: &PeerSummary{Text: "peer notes", Through: 5}}); err != nil {
			t.Fatal(err)
		}
		if len(a.state.Context) != 3 {
			t.Fatalf("setup: %d messages, want the late entry removed", len(a.state.Context))
		}
		if !a.applyReadyPrecompress(context.Background()) || a.state.Summary != "the notes" {
			t.Fatalf("the offer was not applied: summary %q", a.state.Summary)
		}
	})

	t.Run("reload over thread entries", func(t *testing.T) {
		a := offer(t, importedMsg(1, "the input"), own("step one"))
		data, err := json.Marshal(a.state)
		if err != nil {
			t.Fatal(err)
		}
		var st AgentState
		if err := json.Unmarshal(data, &st); err != nil {
			t.Fatal(err)
		}
		if _, isFloat := st.Context[0].Metadata[ThreadSeqMetadataKey].(float64); !isFloat {
			t.Fatal("setup: want the reloaded seq as a float64")
		}
		b := NewUnifiedAgent("edge", "sys", &seamModel{tokens: charTokens}, WithState(&st))
		if !b.applyReadyPrecompress(context.Background()) || b.state.Summary != "the notes" {
			t.Fatalf("the reloaded offer was not applied: summary %q", b.state.Summary)
		}
	})
}

// TestOffersAreOffWithContentBlockCompaction: like background precompression,
// offers are off under the block-level splitter. OfferSummary refuses, and an
// offer loaded from a checkpoint is dropped instead of applied.
func TestOffersAreOffWithContentBlockCompaction(t *testing.T) {
	opt := SummaryApplyOptions{MinTailTokens: 250, MinTailTextMessages: 1, MaxTailTokens: 1000}
	a, snap, _, _ := tailFixture(t)
	a.contentBlockCompaction = true
	if err := a.OfferSummary(context.Background(), snap, "the notes", opt); !errors.Is(err, errOfferWithContentBlocks) {
		t.Fatalf("err = %v, want the content-block refusal", err)
	}
	if a.state.SummaryOffer != nil || a.pendingPrecompress != nil {
		t.Fatal("a refused offer must not be held")
	}

	b, snapB, _, _ := tailFixture(t)
	if err := b.OfferSummary(context.Background(), snapB, "the notes", opt); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(b.state)
	if err != nil {
		t.Fatal(err)
	}
	var st AgentState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	c := NewUnifiedAgent("edge", "sys", &seamModel{tokens: charTokens}, WithState(&st), WithContentBlockCompaction())
	if c.applyReadyPrecompress(context.Background()) {
		t.Fatal("a loaded offer was applied under content-block compaction")
	}
	if c.state.SummaryOffer != nil || c.state.Summary != "" || len(c.state.Context) != len(st.Context) {
		t.Fatalf("offer %+v, summary %q, %d messages", c.state.SummaryOffer, c.state.Summary, len(c.state.Context))
	}
}

// TestContextDigest: fields are framed so no value shifts a boundary, a nil
// message, an absent seq and a seq of 0 all differ, a JSON round trip keeps
// the digest, and hashing allocates per call, not per message.
func TestContextDigest(t *testing.T) {
	msg := func(name, id string) *message.Msg {
		m := message.UserMsg(name, "x")
		m.ID = id
		return m
	}
	digest := func(msgs ...*message.Msg) string { return contextDigest(msgs) }
	if digest(msg("ab", "c")) == digest(msg("a", "bc")) {
		t.Fatal("a value shifted a field boundary")
	}
	if digest(nil) == digest(msg("", "")) || digest() == digest(nil) {
		t.Fatal("a nil message must count, and differ from an empty one")
	}
	if digest(msg("a", "b")) == digest(importedMsg(0, "x")) || digest(importedMsg(0, "x")) == digest(importedMsg(1, "x")) {
		t.Fatal("the seq must count")
	}

	msgs := []*message.Msg{importedMsg(1, "the input"), msg("a", "reply-1"), nil}
	data, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded []*message.Msg
	if err := json.Unmarshal(data, &reloaded); err != nil {
		t.Fatal(err)
	}
	if contextDigest(reloaded) != contextDigest(msgs) {
		t.Fatal("a JSON round trip changed the digest")
	}

	many := make([]*message.Msg, 200)
	for i := range many {
		many[i] = importedMsg(uint64(i), "x")
	}
	one, all := testing.AllocsPerRun(20, func() { contextDigest(many[:1]) }), testing.AllocsPerRun(20, func() { contextDigest(many) })
	if all > one+2 {
		t.Fatalf("allocations grow with the messages: %v for one, %v for 200", one, all)
	}
}

// growingReply runs one reply of three rounds on a single assistant message:
// a long first step that calls echo, a second step that calls echo again,
// then the final answer. atFirstStep runs at boundary 2, right after the
// first step. Every round lands on the same message (saveToContext), so a
// snapshot taken there has that growing message as its boundary, and with a
// 1500-token window the message alone exceeds the default MaxTailTokens
// (window/5).
func growingReply(t *testing.T, atFirstStep func(a *UnifiedAgent)) *UnifiedAgent {
	t.Helper()
	echo := tool.NewFunctionTool("echo", "e", nil, func(context.Context, map[string]any) (any, error) { return "echoed", nil })
	m := &seamModel{tokens: charTokens, respond: func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		switch call {
		case 0:
			r := seamToolCalls(message.ToolCallBlock{ID: "c1", Name: "echo"})
			r.Content = append([]message.ContentBlock{message.TextBlock{Type: "text", Text: strings.Repeat("first step ", 40)}}, r.Content...)
			return r, nil
		case 1:
			r := seamToolCalls(message.ToolCallBlock{ID: "c2", Name: "echo"})
			r.Content = append([]message.ContentBlock{message.TextBlock{Type: "text", Text: "SECOND STEP"}}, r.Content...)
			return r, nil
		}
		return seamText("FINAL ANSWER"), nil
	}}
	var a *UnifiedAgent
	boundary := 0
	src := func(context.Context) (*ThreadUpdate, error) {
		if boundary++; boundary == 2 {
			atFirstStep(a)
		}
		return nil, nil
	}
	a = NewUnifiedAgent("a", "sys", m, WithToolkit(tool.NewToolkit(echo)), WithBoundaryInput(src), WithForkCapture(),
		WithContextConfig(&ContextConfig{ContextSize: 1500}))
	if _, err := a.Reply(context.Background(), "the reply input"); err != nil {
		t.Fatal(err)
	}
	return a
}

// keepsLaterRounds fails unless the context still holds the reply's later
// rounds, which no summary taken after the first step covers.
func keepsLaterRounds(t *testing.T, a *UnifiedAgent) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	var text strings.Builder
	for _, m := range a.state.Context {
		text.WriteString(msgText(m))
	}
	if !strings.Contains(text.String(), "SECOND STEP") || !strings.Contains(text.String(), "FINAL ANSWER") {
		t.Fatalf("the later rounds were dropped: %d messages left, summary %q", len(a.state.Context), a.state.Summary)
	}
}

// TestOfferKeepsTheGrowingBoundaryMessage: an offer made after a reply's
// first step, whose boundary message alone exceeds MaxTailTokens, must keep
// that message when the trigger applies it: the later rounds that grew it
// are not in the summary.
func TestOfferKeepsTheGrowingBoundaryMessage(t *testing.T) {
	var offerErr error
	a := growingReply(t, func(a *UnifiedAgent) {
		snap, err := a.ForkSnapshot()
		if err != nil {
			offerErr = err
			return
		}
		offerErr = a.OfferSummary(context.Background(), snap, "notes on the first step", SummaryApplyOptions{})
	})
	if offerErr != nil {
		t.Fatal(offerErr)
	}
	if _, err := a.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.state.Summary != "notes on the first step" {
		t.Fatalf("summary %q, want the offer applied", a.state.Summary)
	}
	keepsLaterRounds(t, a)
}

// TestApplySummaryKeepsTheGrowingBoundaryMessage: the same with a snapshot
// taken mid-reply and applied after the reply.
func TestApplySummaryKeepsTheGrowingBoundaryMessage(t *testing.T) {
	var snap *ForkSnapshot
	var snapErr error
	a := growingReply(t, func(a *UnifiedAgent) { snap, snapErr = a.ForkSnapshot() })
	if snapErr != nil {
		t.Fatal(snapErr)
	}
	if err := a.ApplySummary(context.Background(), snap, "notes on the first step", SummaryApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	keepsLaterRounds(t, a)
}

// TestHeldSnapshotRejectedWhenARemovalShiftsItsBoundary: the boundary-shift
// case with a snapshot the host holds and applies after the reply.
// ApplySummary must find it stale (ForkSnapshot.validLocked's digest check).
func TestHeldSnapshotRejectedWhenARemovalShiftsItsBoundary(t *testing.T) {
	echo := tool.NewFunctionTool("echo", "e", nil, func(context.Context, map[string]any) (any, error) { return "echoed", nil })
	m := &seamModel{tokens: charTokens, respond: func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		switch call {
		case 0:
			r := seamToolCalls(message.ToolCallBlock{ID: "c1", Name: "echo"})
			r.Content = append([]message.ContentBlock{message.TextBlock{Type: "text", Text: "step one"}}, r.Content...)
			return r, nil
		case 1:
			return seamToolCalls(message.ToolCallBlock{ID: "c2", Name: "echo"}), nil
		}
		return seamText("done"), nil
	}}
	var a *UnifiedAgent
	var snap *ForkSnapshot
	var snapErr error
	boundary := 0
	src := func(context.Context) (*ThreadUpdate, error) {
		boundary++
		switch boundary {
		case 2:
			snap, snapErr = a.ForkSnapshot()
			return &ThreadUpdate{Messages: []*message.Msg{importedMsg(2, "edge note")}, Cursor: 2}, nil
		case 3:
			return &ThreadUpdate{PeerSummary: &PeerSummary{Text: "peer notes", Origin: "cloud", Through: 2}}, nil
		}
		return nil, nil
	}
	a = NewUnifiedAgent("a", "sys", m, WithToolkit(tool.NewToolkit(echo)), WithBoundaryInput(src), WithForkCapture(),
		WithContextConfig(&ContextConfig{ContextSize: 100000}))
	if _, err := a.Reply(WithThreadInput(context.Background(), 1), "the reply input"); err != nil {
		t.Fatal(err)
	}
	if snapErr != nil {
		t.Fatal(snapErr)
	}
	a.mu.Lock()
	shifted := len(a.state.Context) == 2 && a.state.Context[1].ID == a.state.Context[0].ID
	a.mu.Unlock()
	if !shifted {
		t.Fatal("setup: want [A1, A2] sharing the reply ID")
	}
	if err := a.ApplySummary(context.Background(), snap, "summary of the reply input", SummaryApplyOptions{}); !errors.Is(err, ErrStaleSnapshot) {
		t.Fatalf("err = %v, want ErrStaleSnapshot", err)
	}
	if a.state.Summary != "" || len(a.state.Context) != 2 {
		t.Fatalf("state changed: summary %q, %d messages", a.state.Summary, len(a.state.Context))
	}
}

// TestForkSnapshotRejectsARemovalAfterTheCapture: a removal between the
// capture and ForkSnapshot can leave a message with the captured boundary's
// ID at its index; the capture's digest check must refuse the snapshot.
func TestForkSnapshotRejectsARemovalAfterTheCapture(t *testing.T) {
	a := NewUnifiedAgent("edge", "sys", &seamModel{tokens: charTokens}, WithForkCapture())
	own := func(text string) *message.Msg {
		m := message.AssistantMsg("edge", text)
		m.ID = "reply-1"
		return m
	}
	a.state.Context = []*message.Msg{importedMsg(1, "the input"), own("step one")}
	a.captureForkRequest(&middleware.ModelCallInput{Messages: a.prepareModelInput(context.Background())})
	a.state.Context = append(a.state.Context, importedMsg(2, "edge note"), own("step two"))
	if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{PeerSummary: &PeerSummary{Text: "peer notes", Through: 2}}); err != nil {
		t.Fatal(err)
	}
	if len(a.state.Context) != 2 || a.state.Context[1].ID != "reply-1" {
		t.Fatalf("setup: %d messages, want [A1, A2]", len(a.state.Context))
	}
	if _, err := a.ForkSnapshot(); !errors.Is(err, ErrNoForkSnapshot) {
		t.Fatalf("err = %v, want ErrNoForkSnapshot", err)
	}
}
