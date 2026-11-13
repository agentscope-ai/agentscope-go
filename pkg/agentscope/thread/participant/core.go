package participant

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

var (
	// ErrBusy is returned when an activation, or an operation that must run
	// between activations (Sync, Compact), is already running.
	ErrBusy = errors.New("participant: busy")
	// ErrNotBound is returned by Activate before Bind.
	ErrNotBound = errors.New("participant: no host bound")
	// ErrPartialRound is set on ActivationEnd.Err when the reply completed but
	// the provider reported its last model round as partial. The text shown
	// so far is still in ActivationEnd.Text.
	ErrPartialRound = errors.New("participant: the final model round is incomplete")
)

// Participant states.
const (
	stateIdle = iota
	stateActive
	stateOp
)

const defaultMaxEventOutput = 64 << 10

// core is what AgentParticipant and TeamParticipant share: the descriptor,
// the configuration, the host and the current activation.
type core struct {
	desc thread.Descriptor
	// prepared is set when the participant installs its own agent options
	// (boundary input and loop hook); their presence is then checked.
	prepared bool
	toolSet  map[string]bool
	hooks    []loop.Hook
	tools    []tool.Tool
	hook     *threadHook

	capture   bool
	maxOutput int

	mu    sync.Mutex
	host  thread.Host
	state int
	act   *activation
}

func newCore(d thread.Descriptor, opts PrepareOptions, prepared bool) (*core, error) { //nolint:gocritic // the descriptor is copied on purpose
	if err := d.Validate(); err != nil {
		return nil, err
	}
	c := &core{desc: d, prepared: prepared, toolSet: map[string]bool{}}
	if !prepared {
		return c, nil
	}
	names := opts.Tools
	if names == nil {
		for _, n := range allToolNames {
			if (n == ToolPublishArtifact && d.Zone == thread.ZoneEdge) || n == ToolEventRead {
				continue
			}
			names = append(names, n)
		}
	}
	for _, n := range names {
		if !slices.Contains(allToolNames, n) {
			return nil, fmt.Errorf("participant: unknown thread tool %q", n)
		}
		c.toolSet[n] = true
	}
	c.hooks = append([]loop.Hook(nil), opts.LoopHooks...)
	c.hook = &threadHook{c: c}
	for _, n := range allToolNames {
		if c.toolSet[n] {
			c.tools = append(c.tools, newThreadTool(c, n))
		}
	}
	return c, nil
}

// configure records the participant's event capture.
func (c *core) configure(capture *bool, maxOutput int) {
	c.capture = false
	if capture != nil {
		c.capture = *capture
	}
	c.maxOutput = maxOutput
	if c.maxOutput <= 0 {
		c.maxOutput = defaultMaxEventOutput
	}
}

// options returns the agent options a prepared participant's agent needs.
func (c *core) options() []agent.AgentOption {
	opts := []agent.AgentOption{
		agent.WithBoundaryInput(c.boundary),
		agent.WithLoopHooks(append(append([]loop.Hook(nil), c.hooks...), c.hook)...),
	}
	if c.toolSet[ToolTransfer] {
		opts = append(opts, agent.WithExclusiveTools(ToolTransfer))
	}
	if c.desc.Zone == thread.ZoneCloud {
		opts = append(opts, agent.WithForkCapture())
	}
	return opts
}

// hasWriteTools reports whether an external thread tool is installed; those
// are answered through the agent's external-result channel, which only an
// agent with a permission context has.
func (c *core) hasWriteTools() bool {
	for n := range writeTools {
		if c.toolSet[n] {
			return true
		}
	}
	return false
}

func (c *core) bind(h thread.Host) error {
	if h == nil {
		return errors.New("participant: nil host")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.host != nil {
		return fmt.Errorf("participant %s: Bind was already called", c.desc.Name)
	}
	c.host = h
	return nil
}

// release forgets h when it is the bound host, so the participant can be
// bound again. Activations already running keep the host they started with.
func (c *core) release(h thread.Host) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if h != nil && c.host == h {
		c.host = nil
	}
}

func (c *core) hostOf() thread.Host {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.host
}

// beginOp claims the participant for an operation between activations.
func (c *core) beginOp() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != stateIdle {
		return fmt.Errorf("%w: %s has an activation or operation in progress", ErrBusy, c.desc.Name)
	}
	c.state = stateOp
	return nil
}

func (c *core) endOp() {
	c.mu.Lock()
	c.state = stateIdle
	c.mu.Unlock()
}

// beginActivation claims the participant for act and returns the host.
func (c *core) beginActivation(act *activation) (thread.Host, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.host == nil {
		return nil, ErrNotBound
	}
	if c.state != stateIdle {
		return nil, fmt.Errorf("%w: %s has an activation or operation in progress", ErrBusy, c.desc.Name)
	}
	c.state = stateActive
	c.act = act
	return c.host, nil
}

func (c *core) endActivation() {
	c.mu.Lock()
	c.act = nil
	c.state = stateIdle
	c.mu.Unlock()
}

func (c *core) current() *activation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.act
}

func (c *core) busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == stateActive
}

// activation is the state of one running activation that the event
// forwarder, the boundary source, the loop hook and the thread tools share.
type activation struct {
	epoch      uint64
	depth      int
	foreground bool
	triggerSeq uint64
	// ctx is the reply context; host calls made for the activation use it.
	ctx context.Context

	mu            sync.Mutex
	hookStarted   bool
	boundaryCalls int
	cursor        uint64
	confirms      int // confirmation requests not answered yet
}

func newActivation(act *thread.Activation) *activation {
	return &activation{
		epoch:      act.Epoch,
		depth:      act.Depth,
		foreground: act.Foreground,
		triggerSeq: act.Trigger.Seq,
		ctx:        context.Background(),
	}
}

// start records the agent's thread cursor after the activation's own Sync
// was applied.
func (a *activation) start(view agent.ThreadView) { //nolint:gocritic // ThreadView is returned by value
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cursor = max(view.Cursor, a.triggerSeq)
}

func (a *activation) addConfirm() {
	a.mu.Lock()
	a.confirms++
	a.mu.Unlock()
}

func (a *activation) takeConfirm() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.confirms == 0 {
		return false
	}
	a.confirms--
	return true
}

// boundary is the agent's boundary input (agent.WithBoundaryInput). It runs
// on the agent's reply goroutine at every certified boundary, and only acts
// during this participant's own activations.
func (c *core) boundary(ctx context.Context) (*agent.ThreadUpdate, error) {
	act := c.current()
	if act == nil {
		return nil, nil
	}
	act.mu.Lock()
	act.boundaryCalls++
	hookStarted, cursor := act.hookStarted, act.cursor
	act.mu.Unlock()
	if c.prepared && !hookStarted {
		return nil, fmt.Errorf("participant %s: the agent was built without the participant's loop hook; "+
			"apply Prepared.Options() after any WithLoopHooks and pass other hooks through PrepareOptions.LoopHooks", c.desc.Name)
	}
	host := c.hostOf()
	s, err := host.Pending(ctx, c.desc.Name, cursor)
	if err != nil {
		return nil, fmt.Errorf("participant %s: pending thread input: %w", c.desc.Name, err)
	}
	var u *agent.ThreadUpdate
	if !s.Empty() {
		if s.Reset {
			return nil, fmt.Errorf("participant %s: the host returned a reset inside a reply", c.desc.Name)
		}
		tu, err := toUpdate(s)
		if err != nil {
			return nil, fmt.Errorf("participant %s: pending thread input: %w", c.desc.Name, err)
		}
		u = &tu
	}
	act.mu.Lock()
	defer act.mu.Unlock()
	if u != nil && u.Cursor > act.cursor {
		act.cursor = u.Cursor
	}
	return u, nil
}

// errEmptySummary is wrapped by toUpdate for a peer summary entry without
// text. The agent refuses such a summary (it would drop the messages the
// summary covers and put nothing in their place); failing the whole Sync
// keeps a clear from emptying the context around a summary that says nothing.
var errEmptySummary = errors.New("participant: the peer summary entry has no text")

// toUpdate converts a thread Sync into the agent's update. The pointers it
// carries are copied. A peer summary entry whose text is empty or blank fails
// the conversion, so nothing of the Sync is applied.
func toUpdate(s *thread.Sync) (agent.ThreadUpdate, error) {
	if e := s.PeerSummary; e != nil && strings.TrimSpace(e.Text()) == "" {
		return agent.ThreadUpdate{}, fmt.Errorf("%w (#%d)", errEmptySummary, e.Seq)
	}
	u := agent.ThreadUpdate{
		Reset:    s.Reset,
		Messages: append([]*message.Msg(nil), s.Messages...),
		Cursor:   s.Cursor,
	}
	if e := s.PeerSummary; e != nil {
		ps := &agent.PeerSummary{Text: e.Text(), Origin: e.Origin.String()}
		if e.Coverage != nil {
			ps.Through = e.Coverage.Through
			ps.Excluded = append([]uint64(nil), e.Coverage.Excluded...)
		}
		u.PeerSummary = ps
	}
	return u, nil
}

// threadHook is the loop hook a prepared participant installs. It records
// that the agent's loop started, so the boundary can tell that the hook is
// installed.
type threadHook struct{ c *core }

var _ loop.Hook = (*threadHook)(nil)

func (h *threadHook) BeforeModelCall(protocol.LoopState, int)                       {}
func (h *threadHook) AfterModelCall(protocol.LoopState, int, error)                 {}
func (h *threadHook) BeforeToolExec(protocol.LoopState, int, string)                {}
func (h *threadHook) AfterToolExec(protocol.LoopState, int, string, error)          {}
func (h *threadHook) OnStateTransition(protocol.LoopState, protocol.LoopState, int) {}
func (h *threadHook) OnLoopEnd(error)                                               {}

func (h *threadHook) OnLoopStart() {
	if act := h.c.current(); act != nil {
		act.mu.Lock()
		act.hookStarted = true
		act.mu.Unlock()
	}
}
