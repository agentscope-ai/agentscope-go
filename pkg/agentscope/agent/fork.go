package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// Fork summaries (EXPERIMENTAL), modeled on forked agents: a summary request
// is the main thread's last model request plus one appended user message,
// sent with the same system prompt, tools and options so the provider can
// serve the shared prefix from its prompt cache. The main thread is never
// touched: capture copies the request, ForkSnapshot copies the delta and
// ForkSummary only reads the snapshot.

// ErrNoForkSnapshot (EXPERIMENTAL) reports that no safe fork snapshot is
// available.
var ErrNoForkSnapshot = errors.New("agent: no fork snapshot available")

// ErrStaleSnapshot (EXPERIMENTAL) reports that a snapshot no longer matches
// the agent state.
var ErrStaleSnapshot = errors.New("agent: snapshot no longer matches the agent state")

// WithForkCapture (EXPERIMENTAL) enables fork capture: the core model-call
// handler (after middleware, on the buffered and streaming paths) records a
// copy of every request it is about to send, replacing the previous one.
// Without it nothing is captured and nothing is allocated.
func WithForkCapture() AgentOption {
	return func(a *UnifiedAgent) { a.forkCaptureOn = true }
}

// forkCapture is the last captured model request plus the identity of the
// context boundary it was built from. It is immutable once stored.
type forkCapture struct {
	messages       []*message.Msg
	tools          []model.ToolSchema
	toolChoice     *model.ToolChoice
	responseFormat *model.ResponseFormat

	boundary       *message.Msg // last Context message at capture (nil when empty)
	boundaryID     string
	boundaryBlocks int
	contextLen     int
	digest         string // contextDigest of Context[:contextLen]
	summary        string
	at             time.Time
	replyID        string
}

// captureForkRequest records the request the core handler is about to send.
// It runs on the reply goroutine, the only writer of Context during a reply.
func (a *UnifiedAgent) captureForkRequest(input *middleware.ModelCallInput) {
	c := &forkCapture{
		messages:       cloneMsgs(input.Messages),
		tools:          append([]model.ToolSchema(nil), input.Tools...),
		responseFormat: a.responseFormat,
		at:             time.Now(),
	}
	if input.ToolChoice != nil {
		tc := *input.ToolChoice
		tc.Tools = append([]string(nil), tc.Tools...)
		c.toolChoice = &tc
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c.contextLen = len(a.state.Context)
	if c.contextLen > 0 {
		last := a.state.Context[c.contextLen-1]
		c.boundary = last
		if last != nil {
			c.boundaryID = last.ID
			c.boundaryBlocks = len(last.Content)
		}
	}
	c.digest = contextDigest(a.state.Context)
	c.summary = a.state.Summary
	c.replyID = a.state.ReplyID
	a.lastCapture = c
}

// ForkSnapshot (EXPERIMENTAL) is a forkable copy of the agent's latest model
// request plus everything appended to the context since.
type ForkSnapshot struct {
	// Messages is the captured request followed by the delta since capture.
	// The agent's own appended content forms ONE assistant message; any other
	// message appended since (steered or imported input) is copied in order
	// with its role, so input is never presented as the agent's own words.
	Messages []*message.Msg
	// Tools are the tool schemas the captured request carried.
	Tools []model.ToolSchema

	toolChoice     *model.ToolChoice
	responseFormat *model.ResponseFormat
	summary        string // agent summary at capture (and at snapshot)
	// Snapshot-time context boundary: the summary built from Messages covers
	// Context[:contextLen].
	boundary   *message.Msg
	boundaryID string
	contextLen int
	digest     string // contextDigest of Context[:contextLen]
	capturedAt time.Time
	replyID    string
}

// ForkSnapshot (EXPERIMENTAL) returns the captured request plus the delta
// since capture. It fails with ErrNoForkSnapshot (wrapped with the reason)
// when nothing was captured, when the captured boundary message is no longer
// at its index or a message before it was removed, inserted or replaced (a
// peer summary removing thread entries, for one), when the agent's Summary
// changed since the capture (a compaction ran), or when a tool call in the
// delta still lacks its result (an open call is not a safe boundary). It may
// be called while a reply is running; it only reads state under the lock and
// returns copies.
func (a *UnifiedAgent) ForkSnapshot() (*ForkSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.lastCapture
	if c == nil {
		if !a.forkCaptureOn {
			return nil, fmt.Errorf("%w: fork capture is not enabled (WithForkCapture)", ErrNoForkSnapshot)
		}
		return nil, fmt.Errorf("%w: no model request captured yet", ErrNoForkSnapshot)
	}
	if a.state.Summary != c.summary {
		return nil, fmt.Errorf("%w: the summary changed since the capture", ErrNoForkSnapshot)
	}
	live := a.state.Context
	if len(live) < c.contextLen {
		return nil, fmt.Errorf("%w: the captured boundary is no longer in the context", ErrNoForkSnapshot)
	}
	var appended []message.ContentBlock
	if c.contextLen > 0 {
		b := live[c.contextLen-1]
		if b != c.boundary && (b == nil || c.boundaryID == "" || b.ID != c.boundaryID) {
			return nil, fmt.Errorf("%w: the captured boundary message moved", ErrNoForkSnapshot)
		}
		if b != nil {
			if len(b.Content) < c.boundaryBlocks {
				return nil, fmt.Errorf("%w: the captured boundary message was rewritten", ErrNoForkSnapshot)
			}
			appended = b.Content[c.boundaryBlocks:]
			// Only the agent's own assistant message grows in place
			// (saveToContext); anything else growing is not a safe boundary.
			if len(appended) > 0 && (b.Role != message.RoleAssistant || b.Name != a.name) {
				return nil, fmt.Errorf("%w: the captured boundary message was rewritten", ErrNoForkSnapshot)
			}
		}
	}
	if contextDigest(live[:c.contextLen]) != c.digest {
		return nil, fmt.Errorf("%w: the context before the captured boundary changed", ErrNoForkSnapshot)
	}

	var delta []*message.Msg
	own := append([]message.ContentBlock(nil), appended...)
	flush := func() {
		if len(own) > 0 {
			delta = append(delta, message.AssistantMsg(a.name, own))
			own = nil
		}
	}
	for _, m := range live[c.contextLen:] {
		if m == nil {
			continue
		}
		if m.Role == message.RoleAssistant && m.Name == a.name {
			own = append(own, m.Content...)
			continue
		}
		flush()
		delta = append(delta, cloneMsgs([]*message.Msg{m})[0])
	}
	flush()
	if id := openToolCall(delta); id != "" {
		return nil, fmt.Errorf("%w: tool call %q has no result yet", ErrNoForkSnapshot, id)
	}

	snap := &ForkSnapshot{
		Messages:       append(cloneMsgs(c.messages), delta...),
		Tools:          append([]model.ToolSchema(nil), c.tools...),
		toolChoice:     c.toolChoice,
		responseFormat: c.responseFormat,
		summary:        c.summary,
		contextLen:     len(live),
		digest:         contextDigest(live),
		capturedAt:     c.at,
		replyID:        c.replyID,
	}
	if n := len(live); n > 0 && live[n-1] != nil {
		snap.boundary = live[n-1]
		snap.boundaryID = live[n-1].ID
	}
	return snap, nil
}

// openToolCall returns the ID of a tool call in msgs that has no result.
func openToolCall(msgs []*message.Msg) string {
	results := make(map[string]bool)
	for _, m := range msgs {
		for _, b := range m.Content {
			if r, ok := b.(message.ToolResultBlock); ok {
				results[r.ID] = true
			}
		}
	}
	for _, m := range msgs {
		for _, b := range m.Content {
			if call, ok := b.(message.ToolCallBlock); ok && !results[call.ID] {
				return call.ID
			}
		}
	}
	return ""
}

// validLocked reports whether the snapshot still matches the live state: the
// summary is unchanged, the snapshot's last context message still sits at its
// index and the messages up to it are the snapshot's, in order (the
// precompressResult.validLocked rule). Caller holds a.mu.
func (s *ForkSnapshot) validLocked(state *AgentState) bool {
	if state.Summary != s.summary || len(state.Context) < s.contextLen {
		return false
	}
	if s.contextLen == 0 {
		return true
	}
	last := state.Context[s.contextLen-1]
	if last != s.boundary && (last == nil || s.boundaryID == "" || last.ID != s.boundaryID) {
		return false
	}
	return contextDigest(state.Context[:s.contextLen]) == s.digest
}

// ForkRequest (EXPERIMENTAL) configures ForkSummary.
type ForkRequest struct {
	Prompt    string // the summary request, appended as a user message
	Appendix  string // optional, appended after the prompt in the same message (e.g. the previous peer summary)
	MaxTokens int    // output cap for each call; 0 sends none
	// Validate checks a candidate. When it returns ok=false, feedback is sent
	// as a follow-up user message in the same fork (after the assistant's
	// answer, keeping the cached prefix) and the model rewrites, up to
	// MaxRewrites times. Nil accepts everything.
	Validate    func(text string) (feedback string, ok bool)
	MaxRewrites int
	// Guard runs on the final text (the host's guardrail rules). An error
	// fails the fork.
	Guard func(ctx context.Context, text string) error
	// KeepToolChoice sends the captured request's own tool choice instead of
	// "none": whatever the main thread's request sent, and no tool choice at
	// all when it sent none. Some providers (Anthropic among them) drop
	// cached message blocks when tool_choice changes, so keeping it lets the
	// fork reuse the main thread's prompt cache. The model may then call a
	// tool: such a response is treated as a candidate to rewrite. Each tool
	// call is answered with an error result (it never runs), the user
	// message "Do not call tools. Answer with the summary text only."
	// follows in the same fork, and the model answers again. This counts
	// against MaxRewrites; when the rewrites run out the fork fails as it
	// does without KeepToolChoice. A captured choice that forces a tool
	// call ("required" or a named tool) leaves the model no text answer, so
	// such a fork fails. False keeps "none".
	KeepToolChoice bool
}

// forkNoToolsFeedback is the follow-up user message ForkSummary sends when a
// fork made with ForkRequest.KeepToolChoice answered with tool calls.
const forkNoToolsFeedback = "Do not call tools. Answer with the summary text only."

// forkToolNotRunResult is the result recorded for each tool call such a
// response made; the call is never executed.
const forkToolNotRunResult = "Not executed: tools are not available while writing the summary."

// ForkResult (EXPERIMENTAL) is the outcome of ForkSummary.
type ForkResult struct {
	Text     string
	Usage    *model.ChatUsage // summed over every call, cache fields included
	Calls    int              // model calls made, retries included
	Rewrites int
	// Rejected is true when Validate still failed after MaxRewrites; Text is
	// then the last candidate and the caller decides whether to cut it.
	Rejected bool
}

const defaultForkRewriteFeedback = "The summary did not pass validation. Rewrite it."

// ForkSummary (EXPERIMENTAL) asks the agent's model for a summary of snap: the
// snapshot's messages plus one user message holding req.Prompt (and
// req.Appendix), sent with the snapshot's tool schemas, a tool choice of
// "none" (or the captured one with req.KeepToolChoice) and
// model.WithMaxTokens(req.MaxTokens) when positive. It calls the primary
// model's buffered Chat directly: not streaming, not through middleware and
// never the fallback model (a different model breaks the cache and may have a
// smaller window). Transient errors are retried with the reply loop's bounded
// backoff.
//
// The fork fails when a response holds tool calls (with KeepToolChoice, once
// the rewrites run out), carries an error (a partial or incomplete response,
// including a length or content-filter stop) or has no text, and when Guard
// rejects the final text; cancellation returns ctx.Err(). Once a call was
// made the result is returned with the error so the caller can still account
// its usage. ForkSummary never modifies agent state, is safe to call
// concurrently with a running reply and does not record usage into turn
// stats: the caller keeps the ledger.
func (a *UnifiedAgent) ForkSummary(ctx context.Context, snap *ForkSnapshot, req ForkRequest) (*ForkResult, error) {
	if ctx == nil {
		return nil, errors.New("agent: fork summary: nil context")
	}
	if snap == nil || len(snap.Messages) == 0 {
		return nil, fmt.Errorf("%w: empty snapshot", ErrNoForkSnapshot)
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("agent: fork summary: empty prompt")
	}
	prompt := req.Prompt
	if req.Appendix != "" {
		prompt += "\n\n" + req.Appendix
	}
	msgs := make([]*message.Msg, 0, len(snap.Messages)+1+2*max(req.MaxRewrites, 0))
	msgs = append(msgs, snap.Messages...)
	msgs = append(msgs, message.UserMsg(a.name, prompt))

	var opts []model.CallOption
	if len(snap.Tools) > 0 {
		opts = append(opts, model.WithTools(snap.Tools))
	}
	switch {
	case !req.KeepToolChoice:
		opts = append(opts, model.WithToolChoice(&model.ToolChoice{Mode: "none"}))
	case snap.toolChoice != nil:
		tc := *snap.toolChoice
		tc.Tools = append([]string(nil), tc.Tools...)
		opts = append(opts, model.WithToolChoice(&tc))
	}
	opts = ApplyResponseFormat(opts, snap.responseFormat)
	if req.MaxTokens > 0 {
		opts = append(opts, model.WithMaxTokens(req.MaxTokens))
	}

	res := &ForkResult{}
	var text string
	for {
		resp, err := a.forkCall(ctx, msgs, opts, res)
		if err != nil {
			return res, err
		}
		// KeepToolChoice lets the model call a tool; that answer is a
		// candidate to rewrite while rewrites remain.
		if req.KeepToolChoice && forkToolCallTurn(resp) && res.Rewrites < req.MaxRewrites {
			res.Rewrites++
			msgs = append(msgs, forkToolTurn(a.name, resp), message.UserMsg(a.name, forkNoToolsFeedback))
			continue
		}
		if text, err = forkText(resp); err != nil {
			return res, err
		}
		if req.Validate == nil {
			break
		}
		feedback, ok := req.Validate(text)
		if ok {
			break
		}
		if res.Rewrites >= req.MaxRewrites {
			res.Rejected = true
			break
		}
		res.Rewrites++
		if strings.TrimSpace(feedback) == "" {
			feedback = defaultForkRewriteFeedback
		}
		msgs = append(msgs, message.AssistantMsg(a.name, text), message.UserMsg(a.name, feedback))
	}
	if req.Guard != nil {
		if err := req.Guard(ctx, text); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return res, ctxErr
			}
			return res, fmt.Errorf("agent: fork summary rejected by guard: %w", err)
		}
	}
	res.Text = text
	return res, nil
}

// forkCall sends one fork request with callModel's bounded retry policy for
// transient errors, without a fallback model. Every attempt is counted and
// its usage summed into res.
func (a *UnifiedAgent) forkCall(ctx context.Context, msgs []*message.Msg, opts []model.CallOption, res *ForkResult) (*model.ChatResponse, error) {
	attempts := a.modelCfg.MaxRetries
	if attempts <= 0 {
		attempts = defaultModelAttempts
	}
	delay := a.modelCfg.RetryDelay
	if delay <= 0 {
		delay = time.Second
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			timer := time.NewTimer(retryBackoff(delay, i))
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, err := a.model.Chat(ctx, msgs, opts...)
		res.Calls++
		if resp != nil {
			res.Usage = addForkUsage(res.Usage, resp.Usage)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			model.IsContextLimitExceeded(err) || !model.IsRetryableError(err) {
			break
		}
	}
	return nil, fmt.Errorf("agent: fork summary call: %w", lastErr)
}

// forkText extracts the candidate summary from a fork response.
func forkText(resp *model.ChatResponse) (string, error) {
	if resp == nil {
		return "", errors.New("agent: fork summary: model returned no response")
	}
	if resp.Error != nil {
		return "", fmt.Errorf("agent: fork summary: incomplete response: %w", resp.Error)
	}
	if resp.StopReason == model.StopReasonLength || resp.StopReason == model.StopReasonContentFilter {
		return "", fmt.Errorf("agent: fork summary: incomplete response (%s)", resp.StopReason)
	}
	for _, b := range resp.Content {
		if _, ok := b.(message.ToolCallBlock); ok {
			return "", errors.New("agent: fork summary: the model called a tool")
		}
	}
	text := strings.TrimSpace(resp.GetTextContent())
	if text == "" {
		return "", errors.New("agent: fork summary: the model returned no text")
	}
	return text, nil
}

// forkToolCallTurn reports whether resp is a complete response that calls
// tools. A partial, incomplete or empty response is not: forkText fails it.
func forkToolCallTurn(resp *model.ChatResponse) bool {
	if resp == nil || resp.Error != nil ||
		resp.StopReason == model.StopReasonLength || resp.StopReason == model.StopReasonContentFilter {
		return false
	}
	for _, b := range resp.Content {
		if _, ok := b.(message.ToolCallBlock); ok {
			return true
		}
	}
	return false
}

// forkToolTurn is the assistant turn that replays a tool-calling fork
// response: its content followed by an error result for every call, so each
// tool_call keeps its tool_result and the follow-up user message is valid on
// every provider. Nothing is executed.
func forkToolTurn(agentName string, resp *model.ChatResponse) *message.Msg {
	blocks := filterAudioBlocks(resp.Content) // a fresh slice; resp is not changed
	for _, b := range resp.Content {
		if call, ok := b.(message.ToolCallBlock); ok {
			blocks = append(blocks, message.ToolResultBlock{
				Type:   "tool_result",
				ID:     call.ID,
				Name:   call.Name,
				Output: forkToolNotRunResult,
				State:  message.ToolResultError,
			})
		}
	}
	return message.AssistantMsg(agentName, blocks)
}

// addForkUsage adds u into total (allocating it on first use).
func addForkUsage(total, u *model.ChatUsage) *model.ChatUsage {
	if u == nil {
		return total
	}
	if total == nil {
		total = &model.ChatUsage{}
	}
	if total.TotalInputTokens > 0 || u.TotalInputTokens > 0 {
		total.TotalInputTokens = total.EffectiveInputTokens() + u.EffectiveInputTokens()
	}
	total.Time += u.Time
	total.InputTokens += u.InputTokens
	total.OutputTokens += u.OutputTokens
	total.CacheCreationInputTokens += u.CacheCreationInputTokens
	total.CacheInputTokens += u.CacheInputTokens
	return total
}

// SummaryApplyOptions (EXPERIMENTAL) bounds the raw tail ApplySummary keeps
// next to the new summary. Zero fields take the defaults. The token defaults
// are capped by the agent's context window when it is known, so a small window
// keeps a tail it can afford instead of its whole context.
type SummaryApplyOptions struct {
	MinTailTokens       int // default 10000, at most a twentieth of the window
	MinTailTextMessages int // default 5
	MaxTailTokens       int // default 40000, at most a fifth of the window
}

// withDefaults fills the zero fields; window is the agent's context window,
// 0 when unknown.
func (o SummaryApplyOptions) withDefaults(window int) SummaryApplyOptions {
	minTail, maxTail := 10000, 40000
	if window > 0 {
		minTail = min(minTail, max(window/20, 1))
		maxTail = min(maxTail, max(window/5, 1))
	}
	if o.MinTailTokens <= 0 {
		o.MinTailTokens = minTail
	}
	if o.MinTailTextMessages <= 0 {
		o.MinTailTextMessages = 5
	}
	if o.MaxTailTokens <= 0 {
		o.MaxTailTokens = maxTail
	}
	return o
}

// ApplySummary (EXPERIMENTAL) installs text, a summary of the agent's own
// context built from snap (ForkSummary), between replies. Like
// ApplyThreadUpdate it fails, changing nothing, while a reply is in flight
// (including a reply core still running after its stream closed) or a
// Conversation owns the agent. It returns ErrStaleSnapshot and changes nothing
// unless the agent's Summary still equals the one at capture, the snapshot's
// last context message still sits at its index and no message before it was
// removed, inserted or replaced since the snapshot.
// A snapshot taken (ForkSnapshot) while the context was empty covers no
// message to replace: ApplySummary refuses it with an error and changes
// nothing, as OfferSummary does.
//
// The new Context is a tail of the snapshot-time context, grown backwards
// from the snapshot boundary until it holds at least MinTailTokens (by the
// model's CountTokens) and MinTailTextMessages messages with text, never past
// MaxTailTokens, plus every message appended after the boundary. The
// boundary message itself is always kept, even when it alone exceeds
// MaxTailTokens: a reply that runs on after the snapshot adds its later
// rounds to that message, and the summary does not cover them. The kept
// messages are the live ones. The tail never starts on a tool result whose
// call was cut away and never drops a call still awaiting its result (the
// compression path's tool-pair repair), which can also extend it. The
// provider-grounded token floor is reset, a pending background
// precompression is dropped and the read cache is trimmed to the kept
// messages. The result is persisted like ApplyThreadUpdate: a persistence
// error is returned with the state applied. The tail is made of whole
// messages, also under WithContentBlockCompaction: the host chooses when it
// applies.
func (a *UnifiedAgent) ApplySummary(ctx context.Context, snap *ForkSnapshot, text string, opt SummaryApplyOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if snap == nil {
		return errors.New("agent: apply summary: nil snapshot")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("agent: apply summary: empty summary")
	}

	a.mu.Lock()
	var cfgSize int
	if a.contextCfg != nil {
		cfgSize = a.contextCfg.ContextSize
	}
	a.mu.Unlock()
	opt = opt.withDefaults(a.knownWindow(cfgSize))

	a.mu.Lock()
	if err := a.busyLocked("apply summary"); err != nil {
		a.mu.Unlock()
		return err
	}
	if !snap.validLocked(a.state) {
		a.mu.Unlock()
		return ErrStaleSnapshot
	}
	if snap.contextLen == 0 {
		a.mu.Unlock()
		return errors.New("agent: apply summary: the snapshot holds no context to replace")
	}
	live := append([]*message.Msg(nil), a.state.Context...)
	counted := cloneMsgs(live) // counted off-lock; the live pointers stay untouched
	a.mu.Unlock()

	start := a.summaryTailStart(counted, snap.contextLen, opt)

	a.mu.Lock()
	if err := a.busyLocked("apply summary"); err != nil {
		a.mu.Unlock()
		return err
	}
	cur := a.state.Context
	if !snap.validLocked(a.state) || len(cur) != len(live) || (len(cur) > 0 && cur[len(cur)-1] != live[len(live)-1]) {
		a.mu.Unlock()
		return ErrStaleSnapshot
	}
	kept := append([]*message.Msg(nil), cur[start:]...)
	a.state.Context = kept
	a.state.Summary = text
	a.lastModelInputTokens = 0
	a.dropReadyLocked()
	a.compactionVersion++
	a.mu.Unlock()

	if a.readCache != nil {
		cleanReadCacheForReserved(a.readCache, kept)
	}
	return a.persistBetweenReplies(ctx)
}

// SummaryOffer (EXPERIMENTAL) is an offered summary (OfferSummary) as
// AgentState keeps it.
type SummaryOffer struct {
	Text string `json:"text"`
	// Base is the agent's Summary the offer was built on.
	Base string `json:"base,omitempty"`
	// Boundary is the ID of the last context message the summary covers, at
	// index Len-1 of the context. Keep is how many messages up to it stay
	// raw next to the summary: at least 1, the boundary message itself. An
	// offer that keeps none is never applied.
	Boundary string `json:"boundary,omitempty"`
	Len      int    `json:"len"`
	Keep     int    `json:"keep"`
	// Digest fingerprints the order of the first Len context messages (by
	// role, name, ID and thread seq), so an offer whose boundary another
	// message slid onto is not applied. An offer without one is never applied.
	Digest string `json:"digest,omitempty"`
}

// OfferSummary hands the agent text, a summary of its own context built from
// snap (ForkSummary), to install when its context next reaches the
// compression trigger (EXPERIMENTAL). The trigger then applies it without a
// model call, the way it applies a background pre-compression result
// (ContextConfig.PrecompressRatio), and falls back to the synchronous path
// when the offer no longer matches the context or the context is still at
// the threshold once it is in. Unlike ApplySummary it may be called while a
// reply runs, and it changes nothing until the trigger. snap may come from
// another instance built from the same checkpoint: it matches by the IDs
// (with role, name and thread seq) of the messages up to its boundary.
//
// The kept tail is chosen as ApplySummary chooses it, from the
// snapshot-time context; every message appended after the boundary is kept
// as well. A newer offer replaces an older one, and ApplySummary, a strict
// compaction, a thread update that resets the context, or a precompression
// that finds the offer stale drops it. The offer is part of the state
// (AgentState.SummaryOffer), saved here between replies and by the reply's
// own checkpoints otherwise; after a reload it matches the same way. A
// message removed before the boundary (a peer summary removing thread
// entries, for one) makes the offer stale. It returns ErrStaleSnapshot and
// changes nothing unless snap is still valid.
//
// Like background precompression, offers are off under
// WithContentBlockCompaction: the trigger would otherwise keep whole messages
// or split blocks depending on whether an offer happened to be valid when it
// fired. OfferSummary then returns an error, and an offer loaded from a
// checkpoint is dropped at the trigger, which compresses synchronously.
func (a *UnifiedAgent) OfferSummary(ctx context.Context, snap *ForkSnapshot, text string, opt SummaryApplyOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if snap == nil {
		return errors.New("agent: offer summary: nil snapshot")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("agent: offer summary: empty summary")
	}
	if a.contentBlockCompaction {
		return errOfferWithContentBlocks
	}
	a.mu.Lock()
	var cfgSize int
	if a.contextCfg != nil {
		cfgSize = a.contextCfg.ContextSize
	}
	if !snap.validLocked(a.state) {
		a.mu.Unlock()
		return ErrStaleSnapshot
	}
	if snap.contextLen == 0 {
		a.mu.Unlock()
		return errors.New("agent: offer summary: the snapshot holds no context to replace")
	}
	prefix := cloneMsgs(a.state.Context[:snap.contextLen])
	a.mu.Unlock()

	opt = opt.withDefaults(a.knownWindow(cfgSize))
	start := min(a.summaryTailStart(prefix, snap.contextLen, opt), snap.contextLen)

	a.mu.Lock()
	if !snap.validLocked(a.state) {
		a.mu.Unlock()
		return ErrStaleSnapshot
	}
	offer := &SummaryOffer{Text: text, Base: snap.summary, Boundary: snap.boundaryID, Len: snap.contextLen, Keep: snap.contextLen - start, Digest: snap.digest}
	a.state.SummaryOffer = offer
	a.pendingPrecompress = offer.result()
	a.pendingPrecompress.lastMsg = a.state.Context[snap.contextLen-1]
	busy := a.busyLocked("offer summary") != nil
	a.mu.Unlock()
	if busy {
		return nil
	}
	return a.persistBetweenReplies(ctx)
}

// errOfferWithContentBlocks is OfferSummary's refusal under
// WithContentBlockCompaction.
var errOfferWithContentBlocks = errors.New("agent: offer summary: not available with WithContentBlockCompaction")

// result is the pending application the offer stands for.
func (o *SummaryOffer) result() *precompressResult {
	return &precompressResult{prevSummary: o.Base, summary: o.Text, snapshotLen: o.Len, reservedLen: o.Keep,
		lastMsgID: o.Boundary, prefix: o.Digest, offer: true}
}

// summaryTailStart picks the index where ApplySummary's kept tail starts.
// msgs is the full context; boundary is the snapshot-time length. The tail is
// grown backwards from the boundary, then repaired for tool pairs exactly as
// splitMsgsForCompression repairs a compression split. It always keeps the
// boundary message: a snapshot taken mid-reply has the reply's own assistant
// message as its boundary, and the reply's later rounds keep adding blocks to
// it that the summary never saw.
func (a *UnifiedAgent) summaryTailStart(msgs []*message.Msg, boundary int, opt SummaryApplyOptions) int {
	start := boundary
	textMsgs := 0
	for i := boundary - 1; i >= 0; i-- {
		tokens := a.model.CountTokens(msgs[i:boundary], nil)
		if tokens > opt.MaxTailTokens {
			break
		}
		start = i
		if hasText(msgs[i]) {
			textMsgs++
		}
		if tokens >= opt.MinTailTokens && textMsgs >= opt.MinTailTextMessages {
			break
		}
	}
	if start < len(msgs) {
		start = adjustSplitForToolPairs(msgs, start)
	}
	// Clamped after the forward repair, which could otherwise push the start
	// past the boundary again; the backward repair then takes in the call of
	// any result the kept tail holds.
	if boundary > 0 && start >= boundary {
		start = boundary - 1
	}
	return pullSplitBackForToolPairs(msgs, start)
}

func hasText(m *message.Msg) bool {
	if m == nil {
		return false
	}
	for _, b := range m.Content {
		if tb, ok := b.(message.TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
			return true
		}
	}
	return false
}
