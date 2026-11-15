package channel

import (
	"context"
	"strings"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/logging"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Agent is what a gateway session drives — *agent.UnifiedAgent satisfies
// it.
type Agent interface {
	ReplyStream(ctx context.Context, input string) (<-chan event.Event, error)
	SubmitUserConfirm(result *event.UserConfirmResultEvent)
}

// AgentFactory builds (or resumes) the agent for a session key.
type AgentFactory func(sessionKey string) (Agent, error)

// SessionKeyFunc derives the session grouping key from an inbound event.
// The default groups by chat, so one conversation shares one agent.
type SessionKeyFunc func(ev *Event) string

// Notifier is implemented by channels that can push a plain info message
// to a chat outside a reply (the gateway uses it for housekeeping
// notices). Optional.
type Notifier interface {
	Notify(ctx context.Context, chatID string, kind ChatKind, text string) error
}

// GatewayOption configures NewGateway.
type GatewayOption func(*Gateway)

// WithSessionKey overrides session grouping (default: by ChatID).
func WithSessionKey(fn SessionKeyFunc) GatewayOption {
	return func(g *Gateway) {
		if fn != nil {
			g.sessionKey = fn
		}
	}
}

// Gateway orchestrates inbound channel events: routes each message to a
// session agent, runs the reply, tees the event stream to the channel's
// SendResponse, and round-trips tool-call confirmations.
//
// Text-mode confirmation (platforms without an interactive UI): while a
// session has parked confirmations, the next inbound text of y/yes/n/no/
// a(lways) (English or common Chinese equivalents) answers ALL parked
// calls; any other text is dropped with a notice. Channels with native
// confirmation UIs deliver ConfirmationEvent values instead.
//
// One reply runs per session at a time; messages arriving mid-reply are
// dropped with a notice.
type Gateway struct {
	channel    Channel
	newAgent   AgentFactory
	sessionKey SessionKeyFunc

	mu       sync.Mutex
	sessions map[string]*session
}

// NewGateway binds a channel to an agent factory.
func NewGateway(ch Channel, factory AgentFactory, opts ...GatewayOption) *Gateway {
	g := &Gateway{
		channel:    ch,
		newAgent:   factory,
		sessionKey: func(ev *Event) string { return ev.ChatID },
		sessions:   map[string]*session{},
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Start launches the channel listener wired to this gateway. It returns
// once the listener is up; the connection runs until ctx cancellation or
// channel Close.
func (g *Gateway) Start(ctx context.Context) error {
	return g.channel.StartListening(ctx, g.emit)
}

func (g *Gateway) emit(ctx context.Context, in Inbound) error {
	switch {
	case in.Confirmation != nil:
		g.handleConfirmation(ctx, in.Confirmation)
	case in.Message != nil:
		g.handleMessage(ctx, in.Message)
	}
	return nil
}

func (g *Gateway) handleMessage(ctx context.Context, ev *Event) {
	key := g.sessionKey(ev)

	g.mu.Lock()
	sess, ok := g.sessions[key]
	g.mu.Unlock()
	if !ok {
		// Create outside the gateway lock so a slow factory cannot stall
		// inbound handling for every chat (HARNESS R7-LOW-1); on a
		// concurrent race the loser's agent is discarded unused.
		a, err := g.newAgent(key)
		if err != nil {
			logging.Warn("channel: agent factory failed", "session", key, "err", err)
			g.notify(ctx, ev, "❌ The agent could not be started. Please check the configuration.")
			return
		}
		g.mu.Lock()
		if existing, ok2 := g.sessions[key]; ok2 {
			sess = existing
		} else {
			sess = &session{agent: a}
			g.sessions[key] = sess
		}
		g.mu.Unlock()
	}

	sess.mu.Lock()
	if len(sess.parked) > 0 {
		sess.mu.Unlock()
		if approved, withRules, isDecision := parseDecision(ev.Text); isDecision {
			g.applyAll(sess, approved, withRules)
			return
		}
		g.notify(ctx, ev, "⏳ Waiting for your confirmation — reply y (allow) or n (deny).")
		return
	}
	if sess.busy {
		sess.mu.Unlock()
		g.notify(ctx, ev, "⏳ The agent is still replying; please wait.")
		return
	}
	sess.busy = true
	sess.mu.Unlock()

	go g.runReply(ctx, sess, ev)
}

// runReply drives one reply, teeing the agent's event stream to the
// channel and registering parked confirmations along the way.
func (g *Gateway) runReply(ctx context.Context, sess *session, ev *Event) {
	defer func() {
		sess.mu.Lock()
		sess.busy = false
		// Drop any still-parked confirmations: on a normal exit they are
		// already drained, and after an abnormal end (cancel/crash) the
		// dead entries would otherwise brick the session in the
		// "waiting for confirmation" branch forever (HARNESS R7-M1).
		sess.parked = nil
		sess.mu.Unlock()
	}()

	stream, err := sess.agent.ReplyStream(ctx, ev.Text)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		logging.Warn("channel: reply failed to start", "err", err)
		g.notify(ctx, ev, "❌ Agent encountered an error. Please check the agent configuration.")
		return
	}

	// The stream always starts with ReplyStartEvent; read it to name the
	// outbound response.
	var first event.Event
	first, ok := <-stream
	if !ok {
		return
	}
	replyID := first.GetReplyID()

	out := make(chan event.Event, 16)
	rendered := make(chan struct{})
	go func() {
		defer close(rendered)
		if err := g.channel.SendResponse(ctx, Response{
			ReplyID: replyID,
			ChatID:  ev.ChatID,
			Kind:    ev.Kind,
			Events:  out,
		}); err != nil && ctx.Err() == nil {
			logging.Warn("channel: SendResponse failed", "chat", ev.ChatID, "err", err)
		}
	}()

	forward := func(evt event.Event) bool {
		select {
		case out <- evt:
			return true
		case <-ctx.Done():
			return false
		}
	}
	if !forward(first) {
		close(out)
		<-rendered
		return
	}
	for evt := range stream {
		if ce, ok := evt.(event.RequireUserConfirmEvent); ok {
			sess.park(ce.ReplyID, ce.ToolCalls)
		}
		if !forward(evt) {
			break
		}
	}
	close(out)
	<-rendered
}

// handleConfirmation routes a native confirmation (e.g. card click) to
// the session parking the referenced tool call.
func (g *Gateway) handleConfirmation(ctx context.Context, c *ConfirmationEvent) {
	g.mu.Lock()
	var owner *session
	for _, sess := range g.sessions {
		if sess.hasParked(c.ToolCallID) {
			owner = sess
			break
		}
	}
	g.mu.Unlock()

	if owner == nil {
		logging.Warn("channel: confirmation for unknown tool call", "tool_call", c.ToolCallID)
		return
	}
	g.applyOne(owner, c.ToolCallID, c.Approved, c.WithRules)
}

// applyAll answers every parked call of a session (text-mode decision).
func (g *Gateway) applyAll(sess *session, approved, withRules bool) {
	sess.mu.Lock()
	parked := sess.parked
	sess.parked = nil
	sess.mu.Unlock()
	if len(parked) == 0 {
		return
	}
	results := make([]event.ConfirmResult, 0, len(parked))
	for _, p := range parked {
		results = append(results, confirmResultFor(p, approved, withRules))
	}
	sess.agent.SubmitUserConfirm(&event.UserConfirmResultEvent{
		ReplyID:        parked[0].replyID,
		ConfirmResults: results,
	})
}

// applyOne answers a single parked call (native confirmation UI).
func (g *Gateway) applyOne(sess *session, toolCallID string, approved, withRules bool) {
	sess.mu.Lock()
	var target *parkedCall
	var rest []*parkedCall
	for _, p := range sess.parked {
		if p.call.ID == toolCallID {
			target = p
		} else {
			rest = append(rest, p)
		}
	}
	sess.parked = rest
	sess.mu.Unlock()
	if target == nil {
		return
	}
	sess.agent.SubmitUserConfirm(&event.UserConfirmResultEvent{
		ReplyID:        target.replyID,
		ConfirmResults: []event.ConfirmResult{confirmResultFor(target, approved, withRules)},
	})
}

func confirmResultFor(p *parkedCall, approved, withRules bool) event.ConfirmResult {
	cr := event.ConfirmResult{Confirmed: approved, ToolCall: p.call}
	if approved && withRules && len(p.call.SuggestedRules) > 0 {
		cr.Rules = p.call.SuggestedRules
	}
	return cr
}

func (g *Gateway) notify(ctx context.Context, ev *Event, text string) {
	if n, ok := g.channel.(Notifier); ok {
		if err := n.Notify(ctx, ev.ChatID, ev.Kind, text); err != nil {
			logging.Warn("channel: notify failed", "chat", ev.ChatID, "err", err)
		}
		return
	}
	logging.Info("channel: notice (no Notifier support)", "chat", ev.ChatID, "text", text)
}

// parseDecision interprets a text answer to parked confirmations.
func parseDecision(text string) (approved, withRules, ok bool) {
	t := strings.ToLower(strings.TrimSpace(text))
	switch t {
	case "y", "yes", "yes.", "是", "好", "好的", "确认", "同意", "允许":
		return true, false, true
	case "a", "always", "总是", "一直", "始终允许":
		return true, true, true
	case "n", "no", "no.", "否", "不", "拒绝", "取消", "不允许":
		return false, false, true
	}
	return false, false, false
}

// --- session state -------------------------------------------------------

type parkedCall struct {
	replyID string
	call    message.ToolCallBlock
}

type session struct {
	mu     sync.Mutex
	agent  Agent
	busy   bool
	parked []*parkedCall
}

func (s *session) park(replyID string, calls []message.ToolCallBlock) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range calls {
		s.parked = append(s.parked, &parkedCall{replyID: replyID, call: c})
	}
}

func (s *session) hasParked(toolCallID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.parked {
		if p.call.ID == toolCallID {
			return true
		}
	}
	return false
}
